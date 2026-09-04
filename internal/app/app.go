// Package app coordinates one stateless Swarmfolio planning or apply pass.
package app

import (
	"context"
	"errors"
	"fmt"
	"math"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/liblaf/swarmfolio/internal/budget"
	"github.com/liblaf/swarmfolio/internal/config"
	"github.com/liblaf/swarmfolio/internal/disk"
	"github.com/liblaf/swarmfolio/internal/metainfo"
	"github.com/liblaf/swarmfolio/internal/mteam"
	"github.com/liblaf/swarmfolio/internal/optimizer"
	"github.com/liblaf/swarmfolio/internal/qbittorrent"
)

const (
	defaultPollInterval = 250 * time.Millisecond
	defaultPollTimeout  = 15 * time.Second
)

type QBittorrent interface {
	Login(context.Context) error
	Torrents(context.Context) ([]qbittorrent.Torrent, error)
	CategorySavePath(context.Context, string) (string, error)
	DefaultSavePath(context.Context) (string, error)
	FreeSpace(context.Context) (int64, error)
	PreallocateAll(context.Context) (bool, error)
	Add(context.Context, qbittorrent.AddRequest) error
	Delete(context.Context, []string, bool) error
	AddTags(context.Context, []string, []string) error
	RemoveTags(context.Context, []string, []string) error
	Start(context.Context, []string) error
}

type MTeam interface {
	Search(context.Context) ([]mteam.Torrent, error)
	Download(context.Context, int64) ([]byte, error)
}

type ProbeDisk func(string) (disk.Space, error)

type Runner struct {
	Config       config.Settings
	QBittorrent  QBittorrent
	MTeam        MTeam
	ProbeDisk    ProbeDisk
	Now          func() time.Time
	PollInterval time.Duration
	PollTimeout  time.Duration
}

type Report struct {
	Mode                 string        `json:"mode"`
	GeneratedAt          time.Time     `json:"generated_at"`
	ConfigPath           string        `json:"config_path"`
	DownloadPath         string        `json:"download_path"`
	Budget               budget.Result `json:"budget"`
	TorrentCount         int           `json:"torrent_count"`
	CandidateCount       int           `json:"candidate_count"`
	SkippedWithoutExpiry int           `json:"skipped_without_expiry"`
	ProjectedUsedBytes   int64         `json:"projected_used_bytes"`
	Recoveries           []Recovery    `json:"recoveries,omitempty"`
	Actions              []Action      `json:"actions"`
}

type Recovery struct {
	Action string `json:"action"`
	Hash   string `json:"hash"`
	Name   string `json:"name"`
}

type Action struct {
	CandidateID string    `json:"candidate_id"`
	Name        string    `json:"name"`
	SizeBytes   int64     `json:"size_bytes"`
	Seeders     int       `json:"seeders"`
	Leechers    int       `json:"leechers"`
	Opportunity float64   `json:"opportunity"`
	FreeUntil   time.Time `json:"free_until"`
	Removals    []Removal `json:"removals"`
	Applied     bool      `json:"applied"`
}

type Removal struct {
	Hash      string `json:"hash"`
	Name      string `json:"name"`
	SizeBytes int64  `json:"size_bytes"`
}

type snapshot struct {
	all          []qbittorrent.Torrent
	forOptimizer []optimizer.Torrent
	targetPath   string
	budget       budget.Result
}

func (r Runner) Execute(ctx context.Context, apply bool) (Report, error) {
	if r.QBittorrent == nil || r.MTeam == nil {
		return Report{}, errors.New("app: clients are required")
	}
	if r.ProbeDisk == nil {
		r.ProbeDisk = disk.Probe
	}
	if r.Now == nil {
		r.Now = time.Now
	}
	if r.PollInterval <= 0 {
		r.PollInterval = defaultPollInterval
	}
	if r.PollTimeout <= 0 {
		r.PollTimeout = defaultPollTimeout
	}

	now := r.Now()
	report := Report{Mode: "plan", GeneratedAt: now, ConfigPath: r.Config.Path, Actions: []Action{}}
	if apply {
		report.Mode = "apply"
	}
	if err := r.QBittorrent.Login(ctx); err != nil {
		return report, fmt.Errorf("authenticate to qBittorrent: %w", err)
	}

	state, err := r.snapshot(ctx)
	if err != nil {
		return report, err
	}
	results, err := r.MTeam.Search(ctx)
	if err != nil {
		return report, fmt.Errorf("search M-Team freeleech torrents: %w", err)
	}
	report.CandidateCount = len(results)
	candidates, skipped, err := optimizerCandidates(results)
	if err != nil {
		return report, err
	}
	report.SkippedWithoutExpiry = skipped

	var recoveries []Recovery
	for {
		recovered, changed, err := r.recoverPending(ctx, state, candidates, apply)
		if err != nil {
			return report, err
		}
		recoveries = append(recoveries, recovered...)
		if !changed {
			break
		}
		state, err = r.snapshot(ctx)
		if err != nil {
			return report, fmt.Errorf("refresh qBittorrent after pending recovery: %w", err)
		}
	}
	report.Recoveries = recoveries

	report.DownloadPath = state.targetPath
	report.Budget = state.budget
	report.TorrentCount = len(state.all)
	if state.budget.LimitBytes == 0 {
		report.ProjectedUsedBytes = state.budget.UsedBytes
		return report, nil
	}

	plan, err := optimizer.Build(now, candidates, state.forOptimizer, r.optimizerConfig(state.budget.LimitBytes))
	if err != nil {
		return report, fmt.Errorf("build portfolio plan: %w", err)
	}
	report.ProjectedUsedBytes = plan.UsedBytes
	report.Actions = reportActions(plan)
	if !apply {
		return report, nil
	}

	for index := range plan.Additions {
		if err := r.applyAddition(ctx, plan.Additions[index]); err != nil {
			return report, err
		}
		report.Actions[index].Applied = true
	}
	final, err := r.snapshot(ctx)
	if err != nil {
		return report, fmt.Errorf("verify final qBittorrent state: %w", err)
	}
	if final.budget.UsedBytes > final.budget.LimitBytes {
		return report, fmt.Errorf("final portfolio uses %d bytes, exceeding the %d-byte limit", final.budget.UsedBytes, final.budget.LimitBytes)
	}
	report.ProjectedUsedBytes = final.budget.UsedBytes
	return report, nil
}

func (r Runner) snapshot(ctx context.Context) (snapshot, error) {
	torrents, err := r.QBittorrent.Torrents(ctx)
	if err != nil {
		return snapshot{}, fmt.Errorf("list qBittorrent torrents: %w", err)
	}
	targetPath, err := r.QBittorrent.CategorySavePath(ctx, r.Config.QBittorrent.Category)
	if err != nil {
		return snapshot{}, fmt.Errorf("resolve qBittorrent category %q save path: %w", r.Config.QBittorrent.Category, err)
	}
	if !filepath.IsAbs(targetPath) {
		return snapshot{}, fmt.Errorf("qBittorrent category %q save path must be absolute, got %q", r.Config.QBittorrent.Category, targetPath)
	}
	defaultPath := ""
	if r.Config.Portfolio.DiskCapacityBytes > 0 {
		defaultPath, err = r.QBittorrent.DefaultSavePath(ctx)
		if err != nil {
			return snapshot{}, fmt.Errorf("read qBittorrent default save path: %w", err)
		}
	}

	used, outstanding, err := r.account(torrents, targetPath)
	if err != nil {
		return snapshot{}, err
	}
	var space disk.Space
	if r.Config.Portfolio.DiskCapacityBytes > 0 {
		if filepath.Clean(targetPath) != filepath.Clean(defaultPath) {
			return snapshot{}, errors.New("portfolio.disk_capacity requires the qBittorrent category and default save paths to match; use local portfolio.disk_path probing for another filesystem")
		}
		free, err := r.QBittorrent.FreeSpace(ctx)
		if err != nil {
			return snapshot{}, fmt.Errorf("read qBittorrent free disk space: %w", err)
		}
		space = disk.Space{CapacityBytes: r.Config.Portfolio.DiskCapacityBytes, FreeBytes: free}
	} else {
		diskPath := r.Config.Portfolio.DiskPath
		if diskPath == "" {
			diskPath = targetPath
		}
		space, err = r.ProbeDisk(diskPath)
		if err != nil {
			return snapshot{}, fmt.Errorf("probe qBittorrent download disk (set portfolio.disk_path for a container or portfolio.disk_capacity for a remote host): %w", err)
		}
	}
	calculated, err := budget.Calculate(budget.Input{
		CapacityBytes: space.CapacityBytes, FreeBytes: space.FreeBytes,
		UsedBytes: used, OutstandingBytes: outstanding,
		MinimumFreePercent: r.Config.Portfolio.MinimumFreePercent,
		HardLimitBytes:     r.Config.Portfolio.BudgetBytes,
	})
	if err != nil {
		return snapshot{}, err
	}
	converted, err := r.optimizerTorrents(torrents, targetPath)
	if err != nil {
		return snapshot{}, err
	}
	return snapshot{all: torrents, forOptimizer: converted, targetPath: targetPath, budget: calculated}, nil
}

func (r Runner) account(torrents []qbittorrent.Torrent, targetPath string) (int64, int64, error) {
	var sizes, remaining []int64
	for _, torrent := range torrents {
		if torrent.Size < 0 || torrent.AmountLeft < 0 || torrent.AmountLeft > torrent.Size {
			return 0, 0, fmt.Errorf("qBittorrent torrent %q has invalid size or amount_left", torrent.Hash)
		}
		remaining = append(remaining, torrent.AmountLeft)
		if within(targetPath, torrent.SavePath) {
			sizes = append(sizes, torrent.Size)
		}
	}
	used, err := budget.Sum(sizes...)
	if err != nil {
		return 0, 0, err
	}
	outstanding, err := budget.Sum(remaining...)
	if err != nil {
		return 0, 0, err
	}
	return used, outstanding, nil
}

func (r Runner) optimizerTorrents(torrents []qbittorrent.Torrent, targetPath string) ([]optimizer.Torrent, error) {
	out := make([]optimizer.Torrent, 0, len(torrents))
	for _, torrent := range torrents {
		if torrent.Hash == "" || torrent.AddedOn.IsZero() {
			return nil, fmt.Errorf("qBittorrent returned an invalid torrent: hash=%q added_on=%s", torrent.Hash, torrent.AddedOn)
		}
		candidateID, err := candidateID(torrent.Tags, r.Config.QBittorrent.ManagedTag)
		if err != nil {
			return nil, fmt.Errorf("qBittorrent torrent %q: %w", torrent.Hash, err)
		}
		tags := slices.Clone(torrent.Tags)
		size := torrent.Size
		if !within(targetPath, torrent.SavePath) {
			size = 0
			tags = remove(tags, r.Config.QBittorrent.ManagedTag, r.Config.QBittorrent.PendingTag)
		}
		lastActivity := torrent.LastActivity
		if lastActivity.IsZero() {
			lastActivity = torrent.AddedOn
		}
		out = append(out, optimizer.Torrent{
			Hash: strings.ToLower(torrent.Hash), Name: torrent.Name, Size: size,
			Uploaded: torrent.Uploaded, UploadRate: torrent.UPRate, Progress: torrent.Progress,
			State: torrent.State, AddedAt: torrent.AddedOn, LastActivity: lastActivity,
			Tags: strings.Join(tags, ","), CandidateID: candidateID,
			Category: torrent.Category, AutoTMM: torrent.AutoTMM,
		})
	}
	return out, nil
}

func (r Runner) optimizerConfig(limit int64) optimizer.Config {
	return optimizer.Config{
		BudgetBytes: limit, ReserveBytes: 0,
		ManagedTag: r.Config.QBittorrent.ManagedTag, PendingTag: r.Config.QBittorrent.PendingTag,
		Category:              r.Config.QBittorrent.Category,
		ProtectedTags:         slices.Clone(r.Config.QBittorrent.ProtectedTags),
		CandidateMaxAge:       r.Config.Policy.CandidateMaxAge,
		MinFreeleechRemaining: r.Config.Policy.MinimumFreeleechRemaining,
		MinLeechers:           r.Config.Policy.MinimumLeechers,
		MinOpportunityRatio:   r.Config.Policy.MinimumOpportunityRatio,
		MinResidency:          r.Config.Policy.MinimumResidency, MinIdle: r.Config.Policy.MinimumIdle,
		ActiveUploadRate: r.Config.Policy.ActiveUploadRate,
		MaxAdditions:     r.Config.Policy.MaxAdditions, MaxRemovals: r.Config.Policy.MaxRemovals,
	}
}

func (r Runner) recoverPending(ctx context.Context, state snapshot, candidates []optimizer.Candidate, apply bool) ([]Recovery, bool, error) {
	eligible := make(map[string]optimizer.Candidate, len(candidates))
	for _, candidate := range candidates {
		if candidate.FreeUntil.Sub(r.Now()) >= r.Config.Policy.MinimumFreeleechRemaining {
			eligible[candidate.ID] = candidate
		}
	}
	var pending []qbittorrent.Torrent
	for _, torrent := range state.all {
		if !has(torrent.Tags, r.Config.QBittorrent.PendingTag) {
			continue
		}
		if !has(torrent.Tags, r.Config.QBittorrent.ManagedTag) {
			return nil, false, fmt.Errorf("torrent %q has reserved pending tag without managed tag", torrent.Hash)
		}
		pending = append(pending, torrent)
	}
	if len(pending) == 0 {
		return nil, false, nil
	}
	if !apply {
		return nil, false, errors.New("qBittorrent contains an interrupted pending addition; run with --apply to recover it")
	}
	slices.SortFunc(pending, func(a, b qbittorrent.Torrent) int { return strings.Compare(a.Hash, b.Hash) })
	var recoveries []Recovery
	var valid []qbittorrent.Torrent
	var stale []string
	for _, torrent := range pending {
		id, err := candidateID(torrent.Tags, r.Config.QBittorrent.ManagedTag)
		if err != nil {
			return recoveries, false, fmt.Errorf("pending torrent %q: %w", torrent.Hash, err)
		}
		if torrent.Category != r.Config.QBittorrent.Category || !torrent.AutoTMM || !within(state.targetPath, torrent.SavePath) {
			if torrent.Progress > 0 || torrent.AmountLeft != torrent.Size {
				return recoveries, false, fmt.Errorf("pending torrent %q is outside the automatically managed category and contains downloaded data", torrent.Hash)
			}
			stale = append(stale, torrent.Hash)
			recoveries = append(recoveries, Recovery{Action: "remove", Hash: torrent.Hash, Name: torrent.Name})
			continue
		}
		if _, ok := eligible[id]; ok {
			valid = append(valid, torrent)
			continue
		}
		if torrent.Progress > 0 || torrent.AmountLeft != torrent.Size {
			return recoveries, false, fmt.Errorf("pending torrent %q cannot be verified as freeleech and contains downloaded data", torrent.Hash)
		}
		stale = append(stale, torrent.Hash)
		recoveries = append(recoveries, Recovery{Action: "remove", Hash: torrent.Hash, Name: torrent.Name})
	}
	if len(stale) > 0 {
		if err := r.QBittorrent.Delete(ctx, stale, true); err != nil {
			return nil, false, fmt.Errorf("remove unverifiable pending torrents: %w", err)
		}
		return recoveries, true, nil
	}
	pending = valid
	if state.budget.UsedBytes <= state.budget.LimitBytes {
		for _, torrent := range pending {
			if err := r.QBittorrent.Start(ctx, []string{torrent.Hash}); err != nil {
				return recoveries, true, fmt.Errorf("resume pending torrent %q: %w", torrent.Hash, err)
			}
			if err := r.QBittorrent.RemoveTags(ctx, []string{torrent.Hash}, []string{r.Config.QBittorrent.PendingTag}); err != nil {
				return recoveries, true, fmt.Errorf("finalize pending torrent %q: %w", torrent.Hash, err)
			}
			recoveries = append(recoveries, Recovery{Action: "resume", Hash: torrent.Hash, Name: torrent.Name})
		}
		return recoveries, true, nil
	}
	for _, torrent := range pending {
		if torrent.Progress > 0 || torrent.AmountLeft != torrent.Size {
			return recoveries, false, fmt.Errorf("pending torrent %q contains downloaded data while the portfolio is over budget", torrent.Hash)
		}
	}
	hashes := make([]string, len(pending))
	for index, torrent := range pending {
		hashes[index] = torrent.Hash
		recoveries = append(recoveries, Recovery{Action: "remove", Hash: torrent.Hash, Name: torrent.Name})
	}
	if err := r.QBittorrent.Delete(ctx, hashes, true); err != nil {
		return nil, false, fmt.Errorf("remove empty pending torrents: %w", err)
	}
	return recoveries, true, nil
}

func optimizerCandidates(results []mteam.Torrent) ([]optimizer.Candidate, int, error) {
	candidates := make([]optimizer.Candidate, 0, len(results))
	skipped := 0
	for _, result := range results {
		if result.ID <= 0 || result.Seeders < 0 || result.Leechers < 0 || result.Seeders > math.MaxInt || result.Leechers > math.MaxInt {
			return nil, 0, fmt.Errorf("M-Team returned invalid candidate %d", result.ID)
		}
		if result.DiscountEndTime.IsZero() {
			skipped++
			continue
		}
		candidates = append(candidates, optimizer.Candidate{
			ID: strconv.FormatInt(result.ID, 10), Name: result.Name, Size: result.Size,
			Seeders: int(result.Seeders), Leechers: int(result.Leechers),
			PublishedAt: result.PublishedAt, FreeUntil: result.DiscountEndTime,
		})
	}
	return candidates, skipped, nil
}

func reportActions(plan optimizer.Plan) []Action {
	actions := make([]Action, 0, len(plan.Additions))
	for _, addition := range plan.Additions {
		candidate := addition.Candidate
		action := Action{
			CandidateID: candidate.ID, Name: candidate.Name, SizeBytes: candidate.Size,
			Seeders: candidate.Seeders, Leechers: candidate.Leechers,
			Opportunity: (float64(candidate.Leechers) + 1) / (float64(candidate.Seeders) + 1),
			FreeUntil:   candidate.FreeUntil, Removals: make([]Removal, 0, len(addition.Removals)),
		}
		for _, removal := range addition.Removals {
			action.Removals = append(action.Removals, Removal{Hash: removal.Hash, Name: removal.Name, SizeBytes: removal.Size})
		}
		actions = append(actions, action)
	}
	return actions
}

func (r Runner) applyAddition(ctx context.Context, addition optimizer.Addition) error {
	candidate := addition.Candidate
	if candidate.FreeUntil.Sub(r.Now()) < r.Config.Policy.MinimumFreeleechRemaining {
		return fmt.Errorf("candidate %s no longer has the required freeleech time", candidate.ID)
	}
	metainfoBytes, err := r.MTeam.Download(ctx, mustInt64(candidate.ID))
	if err != nil {
		return fmt.Errorf("download M-Team torrent %s: %w", candidate.ID, err)
	}
	hash, err := metainfo.InfoHash(metainfoBytes)
	if err != nil {
		return fmt.Errorf("inspect M-Team torrent %s: %w", candidate.ID, err)
	}
	preallocate, err := r.QBittorrent.PreallocateAll(ctx)
	if err != nil {
		return fmt.Errorf("read qBittorrent preallocation setting: %w", err)
	}
	beforeAdd, err := r.snapshot(ctx)
	if err != nil {
		return fmt.Errorf("refresh qBittorrent before adding candidate %s: %w", candidate.ID, err)
	}
	if err := validatePlannedUsed(beforeAdd, candidate.Size, addition.Removals); err != nil {
		return fmt.Errorf("candidate %s no longer fits the current budget: %w", candidate.ID, err)
	}
	if preallocate && candidate.Size > beforeAdd.budget.FreeBytes-beforeAdd.budget.RequiredFreeBytes {
		return fmt.Errorf("candidate %s cannot be safely preallocated while preserving %d free bytes", candidate.ID, beforeAdd.budget.RequiredFreeBytes)
	}
	if findHash(beforeAdd.all, hash) != nil {
		return fmt.Errorf("candidate %s already exists in qBittorrent as %s", candidate.ID, hash)
	}
	idTag := candidateTag(r.Config.QBittorrent.ManagedTag, candidate.ID)
	if err := r.QBittorrent.Add(ctx, qbittorrent.AddRequest{
		Metainfo: metainfoBytes, MetainfoName: candidate.ID + ".torrent",
		SavePath: beforeAdd.targetPath, Category: r.Config.QBittorrent.Category,
		Tags:    []string{r.Config.QBittorrent.ManagedTag, r.Config.QBittorrent.PendingTag, idTag},
		Stopped: true, AutoTMM: true,
	}); err != nil {
		return fmt.Errorf("add M-Team torrent %s to qBittorrent: %w", candidate.ID, err)
	}
	rollback := func(cause error) error {
		if err := r.QBittorrent.Delete(ctx, []string{hash}, true); err != nil {
			return errors.Join(cause, fmt.Errorf("roll back pending candidate %s: %w", candidate.ID, err))
		}
		return cause
	}
	added, _, err := r.waitForTorrent(ctx, hash)
	if err != nil {
		return rollback(fmt.Errorf("verify added M-Team torrent %s: %w", candidate.ID, err))
	}
	if added.Size != candidate.Size || added.Progress != 0 || added.Category != r.Config.QBittorrent.Category || !added.AutoTMM ||
		!has(added.Tags, r.Config.QBittorrent.ManagedTag) ||
		!has(added.Tags, r.Config.QBittorrent.PendingTag) || !has(added.Tags, idTag) || !within(beforeAdd.targetPath, added.SavePath) {
		return rollback(fmt.Errorf("added M-Team torrent %s does not match its stopped, tagged, automatically managed plan", candidate.ID))
	}
	afterAdd, err := r.snapshot(ctx)
	if err != nil {
		return rollback(fmt.Errorf("refresh qBittorrent after adding candidate %s: %w", candidate.ID, err))
	}
	if err := validatePlannedUsed(afterAdd, 0, addition.Removals); err != nil {
		return rollback(fmt.Errorf("candidate %s no longer fits the current budget: %w", candidate.ID, err))
	}
	if err := r.verifyRemovals(addition.Removals, afterAdd.all); err != nil {
		return rollback(err)
	}
	if len(addition.Removals) > 0 {
		hashes := make([]string, len(addition.Removals))
		for index, removal := range addition.Removals {
			hashes[index] = removal.Hash
		}
		if err := r.QBittorrent.Delete(ctx, hashes, true); err != nil {
			return fmt.Errorf("delete replaced torrents for candidate %s: %w", candidate.ID, err)
		}
	}
	afterDelete, err := r.snapshot(ctx)
	if err != nil {
		return fmt.Errorf("refresh qBittorrent after deleting replacements for candidate %s: %w", candidate.ID, err)
	}
	if err := validatePlannedUsed(afterDelete, 0, nil); err != nil {
		return fmt.Errorf("candidate %s no longer fits the current budget after deletion: %w", candidate.ID, err)
	}
	if err := r.QBittorrent.Start(ctx, []string{hash}); err != nil {
		return fmt.Errorf("start candidate %s: %w", candidate.ID, err)
	}
	if err := r.QBittorrent.RemoveTags(ctx, []string{hash}, []string{r.Config.QBittorrent.PendingTag}); err != nil {
		return fmt.Errorf("finalize candidate %s: %w", candidate.ID, err)
	}
	return nil
}

func validatePlannedUsed(state snapshot, additionSize int64, removals []optimizer.Removal) error {
	removed := make([]int64, 0, len(removals))
	for _, removal := range removals {
		removed = append(removed, removal.Size)
	}
	reclaimed, err := budget.Sum(removed...)
	if err != nil {
		return err
	}
	if additionSize > 0 && state.budget.UsedBytes > math.MaxInt64-additionSize {
		return errors.New("planned portfolio size overflows int64")
	}
	projected := state.budget.UsedBytes + additionSize - reclaimed
	if projected > state.budget.LimitBytes {
		return fmt.Errorf("projected %d bytes exceeds current %d-byte limit", projected, state.budget.LimitBytes)
	}
	return nil
}

func (r Runner) waitForTorrent(ctx context.Context, hash string) (*qbittorrent.Torrent, []qbittorrent.Torrent, error) {
	deadline := time.NewTimer(r.PollTimeout)
	defer deadline.Stop()
	ticker := time.NewTicker(r.PollInterval)
	defer ticker.Stop()
	for {
		torrents, err := r.QBittorrent.Torrents(ctx)
		if err != nil {
			return nil, nil, err
		}
		if torrent := findHash(torrents, hash); torrent != nil {
			return torrent, torrents, nil
		}
		select {
		case <-ctx.Done():
			return nil, nil, ctx.Err()
		case <-deadline.C:
			return nil, nil, fmt.Errorf("torrent %s did not appear within %s", hash, r.PollTimeout)
		case <-ticker.C:
		}
	}
}

func (r Runner) verifyRemovals(removals []optimizer.Removal, current []qbittorrent.Torrent) error {
	now := r.Now()
	for _, removal := range removals {
		torrent := findHash(current, removal.Hash)
		if torrent == nil {
			return fmt.Errorf("planned removal %s disappeared from qBittorrent", removal.Hash)
		}
		if torrent.Size != removal.Size || torrent.Progress != 1 || torrent.Category != r.Config.QBittorrent.Category || !torrent.AutoTMM ||
			!has(torrent.Tags, r.Config.QBittorrent.ManagedTag) ||
			hasAny(torrent.Tags, r.Config.QBittorrent.ProtectedTags) || has(torrent.Tags, r.Config.QBittorrent.PendingTag) ||
			torrent.UPRate > r.Config.Policy.ActiveUploadRate || now.Sub(torrent.AddedOn) < r.Config.Policy.MinimumResidency ||
			(!torrent.LastActivity.IsZero() && now.Sub(torrent.LastActivity) < r.Config.Policy.MinimumIdle) || busyState(torrent.State) {
			return fmt.Errorf("planned removal %s changed and is no longer safe to delete", removal.Hash)
		}
	}
	return nil
}

func busyState(state string) bool {
	state = strings.ToLower(state)
	return strings.Contains(state, "downloading") || strings.Contains(state, "metadl") || strings.Contains(state, "checking") || strings.Contains(state, "moving") || strings.Contains(state, "allocating")
}

func candidateTag(managedTag, id string) string { return managedTag + "-id-" + id }

func candidateID(tags []string, managedTag string) (string, error) {
	prefix := managedTag + "-id-"
	value := ""
	for _, tag := range tags {
		if !strings.HasPrefix(tag, prefix) {
			continue
		}
		id := strings.TrimPrefix(tag, prefix)
		parsed, err := strconv.ParseInt(id, 10, 64)
		if err != nil || parsed <= 0 || (value != "" && value != id) {
			return "", fmt.Errorf("invalid or conflicting candidate ID tag %q", tag)
		}
		value = id
	}
	return value, nil
}

func mustInt64(value string) int64 {
	parsed, err := strconv.ParseInt(value, 10, 64)
	if err != nil || parsed <= 0 {
		panic("optimizer produced an invalid M-Team ID")
	}
	return parsed
}

func findHash(torrents []qbittorrent.Torrent, hash string) *qbittorrent.Torrent {
	for index := range torrents {
		if strings.EqualFold(torrents[index].Hash, hash) {
			return &torrents[index]
		}
	}
	return nil
}

func within(root, child string) bool {
	if root == "" || child == "" {
		return false
	}
	relative, err := filepath.Rel(filepath.Clean(root), filepath.Clean(child))
	return err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))
}

func has(tags []string, wanted string) bool { return slices.Contains(tags, wanted) }

func hasAny(tags, wanted []string) bool {
	for _, tag := range wanted {
		if has(tags, tag) {
			return true
		}
	}
	return false
}

func remove(tags []string, unwanted ...string) []string {
	return slices.DeleteFunc(tags, func(tag string) bool { return slices.Contains(unwanted, tag) })
}
