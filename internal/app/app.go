// Package app coordinates one stateless Swarmfolio planning or apply pass.
package app

import (
	"context"
	"errors"
	"fmt"
	"math"
	"path"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/liblaf/swarmfolio/internal/budget"
	"github.com/liblaf/swarmfolio/internal/config"
	"github.com/liblaf/swarmfolio/internal/disk"
	"github.com/liblaf/swarmfolio/internal/mteam"
	"github.com/liblaf/swarmfolio/internal/optimizer"
	"github.com/liblaf/swarmfolio/internal/qbittorrent"
)

const (
	defaultPollInterval = 250 * time.Millisecond
	defaultPollTimeout  = 15 * time.Second
)

type QBittorrent interface {
	Torrents(context.Context) ([]qbittorrent.Torrent, error)
	CategorySavePath(context.Context, string) (string, error)
	DefaultSavePath(context.Context) (string, error)
	FreeSpace(context.Context) (int64, error)
	PreallocateAll(context.Context) (bool, error)
	Add(context.Context, qbittorrent.AddRequest) error
	Delete(context.Context, []string, bool) error
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
	PlanningHorizon      string        `json:"planning_horizon"`
	ReplacementMargin    float64       `json:"replacement_margin"`
	NetGain              float64       `json:"net_gain_score_bytes"`
	Recoveries           []Recovery    `json:"recoveries,omitempty"`
	Actions              []Action      `json:"actions"`
}

type Recovery struct {
	Action string `json:"action"`
	Hash   string `json:"hash"`
	Name   string `json:"name"`
}

type Action struct {
	CandidateID      string    `json:"candidate_id"`
	Name             string    `json:"name"`
	SizeBytes        int64     `json:"size_bytes"`
	Seeders          int       `json:"seeders"`
	Leechers         int       `json:"leechers"`
	Opportunity      float64   `json:"opportunity"`
	UploadMultiplier int       `json:"upload_multiplier"`
	UploadScore      float64   `json:"upload_score_bytes"`
	FreeUntil        time.Time `json:"free_until"`
	Removals         []Removal `json:"removals"`
	Applied          bool      `json:"applied"`
}

type Removal struct {
	Hash        string  `json:"hash"`
	Name        string  `json:"name"`
	SizeBytes   int64   `json:"size_bytes"`
	UploadScore float64 `json:"upload_score_bytes"`
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
	report := Report{Mode: "plan", GeneratedAt: now, ConfigPath: r.Config.Path, PlanningHorizon: r.Config.Policy.PlanningHorizon.String(), ReplacementMargin: r.Config.Policy.ReplacementMargin, Actions: []Action{}}
	if apply {
		report.Mode = "apply"
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

	resolved := candidateResolver{mteam: r.MTeam, byID: make(map[string]candidateMetainfo)}
	recoveries, changed, err := r.recoverPending(ctx, state, candidates, apply, &resolved)
	if err != nil {
		return report, err
	}
	if changed {
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
		if state.budget.UsedBytes > 0 || state.budget.FreeBytes-state.budget.OutstandingBytes < state.budget.RequiredFreeBytes {
			return report, errors.New("cannot preserve the category disk free-space reserve: no safe portfolio budget remains")
		}
		return report, nil
	}

	plan, err := r.buildPlan(ctx, now, candidates, state, &resolved)
	if err != nil {
		return report, fmt.Errorf("build portfolio plan: %w", err)
	}
	report.ProjectedUsedBytes = plan.UsedBytes
	report.NetGain = plan.NetGain
	report.Actions = reportActions(plan)
	if plan.UsedBytes > plan.LimitBytes {
		return report, fmt.Errorf("no safe plan fits the %d-byte portfolio limit while preserving %d free bytes", plan.LimitBytes, state.budget.RequiredFreeBytes)
	}
	if !apply {
		return report, nil
	}

	for index := range plan.Additions {
		if err := r.applyAddition(ctx, plan.Additions[index], &resolved); err != nil {
			return report, err
		}
		report.Actions[index].Applied = true
	}
	final, err := r.snapshot(ctx)
	if err != nil {
		return report, fmt.Errorf("verify final qBittorrent state: %w", err)
	}
	if final.budget.FreeBytes-final.budget.OutstandingBytes < final.budget.RequiredFreeBytes {
		return report, fmt.Errorf("final category disk cannot preserve %d free bytes after outstanding downloads complete", final.budget.RequiredFreeBytes)
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
	if !remotePathIsAbs(targetPath) {
		return snapshot{}, fmt.Errorf("qBittorrent category %q save path must be absolute, got %q", r.Config.QBittorrent.Category, targetPath)
	}
	used, outstanding, err := r.account(torrents, targetPath)
	if err != nil {
		return snapshot{}, err
	}
	var free int64
	if r.Config.Portfolio.DiskPath != "" {
		space, err := r.ProbeDisk(r.Config.Portfolio.DiskPath)
		if err != nil {
			return snapshot{}, fmt.Errorf("probe portfolio.disk_path: %w", err)
		}
		free = space.FreeBytes
	} else {
		defaultPath, err := r.QBittorrent.DefaultSavePath(ctx)
		if err != nil {
			return snapshot{}, fmt.Errorf("read qBittorrent default save path: %w", err)
		}
		if !within(defaultPath, targetPath) {
			return snapshot{}, errors.New("qBittorrent category must be within the default save path to use its reported free space; set portfolio.disk_path for another filesystem")
		}
		free, err = r.QBittorrent.FreeSpace(ctx)
		if err != nil {
			return snapshot{}, fmt.Errorf("read qBittorrent free disk space: %w", err)
		}
	}
	calculated, err := budget.Calculate(budget.Input{
		FreeBytes: free,
		UsedBytes: used, OutstandingBytes: outstanding,
		MinimumFreeBytes: r.Config.Portfolio.MinimumFreeBytes,
		HardLimitBytes:   r.Config.Portfolio.BudgetBytes,
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
		if torrent.Category == r.Config.QBittorrent.Category && within(targetPath, torrent.SavePath) {
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
		if torrent.Category != r.Config.QBittorrent.Category {
			continue
		}
		if !torrent.AutoTMM {
			return nil, fmt.Errorf("qBittorrent torrent %q in category %q does not use Automatic Torrent Management", torrent.Hash, torrent.Category)
		}
		if !within(targetPath, torrent.SavePath) {
			return nil, fmt.Errorf("qBittorrent torrent %q in category %q is outside its configured save path", torrent.Hash, torrent.Category)
		}
		lastActivity := torrent.LastActivity
		if lastActivity.IsZero() {
			lastActivity = torrent.AddedOn
		}
		out = append(out, optimizer.Torrent{
			Hash: strings.ToLower(torrent.Hash), Name: torrent.Name, Size: torrent.Size,
			Uploaded: torrent.Uploaded, UploadRate: torrent.UPRate, Progress: torrent.Progress,
			State: torrent.State, AddedAt: torrent.AddedOn, LastActivity: lastActivity,
			Category: torrent.Category, AutoTMM: torrent.AutoTMM,
		})
	}
	return out, nil
}

func (r Runner) optimizerConfig(limit int64) optimizer.Config {
	return optimizer.Config{
		BudgetBytes: limit, ReserveBytes: 0,
		Category:              r.Config.QBittorrent.Category,
		CandidateMaxAge:       r.Config.Policy.CandidateMaxAge,
		MinFreeleechRemaining: r.Config.Policy.MinimumFreeleechRemaining,
		MinLeechers:           r.Config.Policy.MinimumLeechers,
		MinOpportunityRatio:   r.Config.Policy.MinimumOpportunityRatio,
		MinResidency:          r.Config.Policy.MinimumResidency, MinIdle: r.Config.Policy.MinimumIdle,
		ActiveUploadRate: r.Config.Policy.ActiveUploadRate,
		MaxAdditions:     r.Config.Policy.MaxAdditions, MaxRemovals: r.Config.Policy.MaxRemovals,
		PlanningHorizon: r.Config.Policy.PlanningHorizon, ReplacementMargin: r.Config.Policy.ReplacementMargin,
	}
}

func (r Runner) recoverPending(ctx context.Context, state snapshot, candidates []optimizer.Candidate, apply bool, resolved *candidateResolver) ([]Recovery, bool, error) {
	var pending []qbittorrent.Torrent
	for _, torrent := range state.all {
		if torrent.Category == r.Config.QBittorrent.Category && torrent.AutoTMM &&
			within(state.targetPath, torrent.SavePath) && emptyStoppedDownload(torrent) {
			pending = append(pending, torrent)
		}
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
	var stale []qbittorrent.Torrent
	for _, torrent := range pending {
		matched, err := r.pendingCandidateIsEligible(ctx, torrent, candidates, resolved)
		if err != nil {
			return recoveries, false, err
		}
		if matched {
			valid = append(valid, torrent)
			continue
		}
		stale = append(stale, torrent)
	}
	if _, err := r.verifyPending(ctx, pending, state.targetPath); err != nil {
		return recoveries, false, err
	}
	if len(stale) > 0 {
		hashes := torrentHashes(stale)
		if err := r.QBittorrent.Delete(ctx, hashes, true); err != nil {
			return nil, false, fmt.Errorf("remove unverifiable pending torrents: %w", err)
		}
		for _, torrent := range stale {
			recoveries = append(recoveries, Recovery{Action: "remove", Hash: torrent.Hash, Name: torrent.Name})
		}
	}
	var err error
	state, err = r.snapshot(ctx)
	if err != nil {
		return recoveries, len(stale) > 0, fmt.Errorf("refresh qBittorrent after checking pending torrents: %w", err)
	}
	if len(valid) == 0 {
		return recoveries, len(stale) > 0, nil
	}
	hashes, err := r.verifyPending(ctx, valid, state.targetPath)
	if err != nil {
		return recoveries, len(stale) > 0, err
	}
	if state.budget.UsedBytes <= state.budget.LimitBytes {
		if err := r.QBittorrent.Start(ctx, hashes); err != nil {
			return recoveries, len(stale) > 0, fmt.Errorf("resume pending torrents: %w", err)
		}
		for _, torrent := range valid {
			recoveries = append(recoveries, Recovery{Action: "resume", Hash: torrent.Hash, Name: torrent.Name})
		}
		return recoveries, true, nil
	}
	if err := r.QBittorrent.Delete(ctx, hashes, true); err != nil {
		return nil, false, fmt.Errorf("remove empty pending torrents: %w", err)
	}
	for _, torrent := range valid {
		recoveries = append(recoveries, Recovery{Action: "remove", Hash: torrent.Hash, Name: torrent.Name})
	}
	return recoveries, true, nil
}

func (r Runner) verifyPending(ctx context.Context, expected []qbittorrent.Torrent, targetPath string) ([]string, error) {
	current, err := r.QBittorrent.Torrents(ctx)
	if err != nil {
		return nil, fmt.Errorf("refresh pending qBittorrent torrents before mutation: %w", err)
	}
	for _, expectedTorrent := range expected {
		torrent := findHash(current, expectedTorrent.Hash)
		if torrent == nil {
			return nil, fmt.Errorf("pending torrent %q disappeared before mutation", expectedTorrent.Hash)
		}
		if !r.isPending(*torrent, targetPath) {
			return nil, fmt.Errorf("pending torrent %q changed and is no longer safe to mutate", expectedTorrent.Hash)
		}
	}
	return torrentHashes(expected), nil
}

func (r Runner) isPending(torrent qbittorrent.Torrent, targetPath string) bool {
	return torrent.Category == r.Config.QBittorrent.Category && torrent.AutoTMM &&
		within(targetPath, torrent.SavePath) && emptyStoppedDownload(torrent)
}

func torrentHashes(torrents []qbittorrent.Torrent) []string {
	hashes := make([]string, len(torrents))
	for index, torrent := range torrents {
		hashes[index] = torrent.Hash
	}
	return hashes
}

func (r Runner) pendingCandidateIsEligible(ctx context.Context, torrent qbittorrent.Torrent, candidates []optimizer.Candidate, resolved *candidateResolver) (bool, error) {
	for _, candidate := range candidates {
		if candidate.Size != torrent.Size ||
			candidate.FreeUntil.Sub(r.Now()) < r.Config.Policy.MinimumFreeleechRemaining {
			continue
		}
		info, err := resolved.resolve(ctx, candidate.ID)
		if err != nil {
			return false, fmt.Errorf("recover pending torrent %s: %w", torrent.Hash, err)
		}
		if strings.EqualFold(info.hash, torrent.Hash) {
			return true, nil
		}
	}
	return false, nil
}

func optimizerCandidates(results []mteam.Torrent) ([]optimizer.Candidate, int, error) {
	candidates := make([]optimizer.Candidate, 0, len(results))
	skipped := 0
	for _, result := range results {
		if result.ID <= 0 || result.Seeders < 0 || result.Leechers < 0 || result.Seeders > math.MaxInt || result.Leechers > math.MaxInt {
			return nil, 0, fmt.Errorf("M-Team returned invalid candidate %d", result.ID)
		}
		multiplier, err := uploadMultiplier(result.Discount)
		if err != nil {
			return nil, 0, err
		}
		if result.DiscountEndTime.IsZero() {
			skipped++
			continue
		}
		candidates = append(candidates, optimizer.Candidate{
			ID: strconv.FormatInt(result.ID, 10), Name: result.Name, Size: result.Size,
			Seeders: int(result.Seeders), Leechers: int(result.Leechers),
			PublishedAt: result.PublishedAt, FreeUntil: result.DiscountEndTime,
			UploadMultiplier: multiplier,
		})
	}
	return candidates, skipped, nil
}

func uploadMultiplier(discount string) (int, error) {
	switch discount {
	case "FREE":
		return 1, nil
	case "_2X_FREE":
		return 2, nil
	default:
		return 0, fmt.Errorf("M-Team returned unsupported freeleech discount %q", discount)
	}
}

func reportActions(plan optimizer.Plan) []Action {
	actions := make([]Action, 0, len(plan.Additions))
	for _, addition := range plan.Additions {
		candidate := addition.Candidate
		action := Action{
			CandidateID: candidate.ID, Name: candidate.Name, SizeBytes: candidate.Size,
			Seeders: candidate.Seeders, Leechers: candidate.Leechers,
			Opportunity:      float64(candidate.Leechers) / (float64(candidate.Seeders) + 1),
			UploadMultiplier: candidate.UploadMultiplier, UploadScore: addition.UploadScore,
			FreeUntil: candidate.FreeUntil, Removals: make([]Removal, 0, len(addition.Removals)),
		}
		for _, removal := range addition.Removals {
			action.Removals = append(action.Removals, Removal{Hash: removal.Hash, Name: removal.Name, SizeBytes: removal.Size, UploadScore: removal.UploadScore})
		}
		actions = append(actions, action)
	}
	return actions
}

func (r Runner) applyAddition(ctx context.Context, addition optimizer.Addition, resolved *candidateResolver) error {
	candidate := addition.Candidate
	if candidate.FreeUntil.Sub(r.Now()) < r.Config.Policy.MinimumFreeleechRemaining {
		return fmt.Errorf("candidate %s no longer has the required freeleech time", candidate.ID)
	}
	info, err := resolved.resolve(ctx, candidate.ID)
	if err != nil {
		return err
	}
	hash := info.hash
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
	if err := r.QBittorrent.Add(ctx, qbittorrent.AddRequest{
		Metainfo: info.bytes, MetainfoName: candidate.ID + ".torrent",
		SavePath: beforeAdd.targetPath, Category: r.Config.QBittorrent.Category,
		Stopped: true, AutoTMM: true,
	}); err != nil {
		return fmt.Errorf("add M-Team torrent %s to qBittorrent: %w", candidate.ID, err)
	}
	rollback := func(cause error) error {
		return r.rollbackPending(ctx, hash, beforeAdd.targetPath, candidate.ID, cause)
	}
	added, _, err := r.waitForTorrent(ctx, hash)
	if err != nil {
		return rollback(fmt.Errorf("verify added M-Team torrent %s: %w", candidate.ID, err))
	}
	if added.Size != candidate.Size || !emptyStoppedDownload(*added) || added.Category != r.Config.QBittorrent.Category ||
		!added.AutoTMM || !within(beforeAdd.targetPath, added.SavePath) {
		return rollback(fmt.Errorf("added M-Team torrent %s does not match its stopped, automatically managed category plan: state=%q size=%d (expected %d) amount_left=%d progress=%g category=%q auto_tmm=%t save_path=%q", candidate.ID, added.State, added.Size, candidate.Size, added.AmountLeft, added.Progress, added.Category, added.AutoTMM, added.SavePath))
	}
	afterAdd, err := r.snapshot(ctx)
	if err != nil {
		return rollback(fmt.Errorf("refresh qBittorrent after adding candidate %s: %w", candidate.ID, err))
	}
	if err := validatePlannedUsed(afterAdd, 0, addition.Removals); err != nil {
		return rollback(fmt.Errorf("candidate %s no longer fits the current budget: %w", candidate.ID, err))
	}
	if err := r.verifyRemovals(addition.Removals, afterAdd.all, afterAdd.targetPath); err != nil {
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
	hashes, err := r.verifyPending(ctx, []qbittorrent.Torrent{{Hash: hash}}, afterDelete.targetPath)
	if err != nil {
		return fmt.Errorf("verify candidate %s before starting it: %w", candidate.ID, err)
	}
	if err := r.QBittorrent.Start(ctx, hashes); err != nil {
		return fmt.Errorf("start candidate %s: %w", candidate.ID, err)
	}
	return nil
}

func (r Runner) rollbackPending(ctx context.Context, hash, targetPath, candidateID string, cause error) error {
	current, err := r.QBittorrent.Torrents(ctx)
	if err != nil {
		return errors.Join(cause, fmt.Errorf("inspect pending candidate %s before rollback: %w", candidateID, err))
	}
	torrent := findHash(current, hash)
	if torrent == nil {
		return cause
	}
	if !r.isPending(*torrent, targetPath) {
		return errors.Join(cause, fmt.Errorf("refuse to roll back candidate %s because its category, Automatic Torrent Management, save path, or stopped-empty state changed", candidateID))
	}
	if err := r.QBittorrent.Delete(ctx, []string{hash}, true); err != nil {
		return errors.Join(cause, fmt.Errorf("roll back pending candidate %s: %w", candidateID, err))
	}
	return cause
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
	lastState := ""
	for {
		torrents, err := r.QBittorrent.Torrents(ctx)
		if err != nil {
			return nil, nil, err
		}
		if torrent := findHash(torrents, hash); torrent != nil {
			lastState = torrent.State
			switch strings.ToLower(torrent.State) {
			case "checkingresumedata", "checkingdl":
				// qBittorrent briefly checks resume data after a stopped add.
				// Wait for its stable state before enforcing Swarmfolio's plan.
			default:
				return torrent, torrents, nil
			}
		}
		select {
		case <-ctx.Done():
			return nil, nil, ctx.Err()
		case <-deadline.C:
			if lastState == "" {
				return nil, nil, fmt.Errorf("torrent %s did not appear within %s", hash, r.PollTimeout)
			}
			return nil, nil, fmt.Errorf("torrent %s did not leave transient checking state %q within %s", hash, lastState, r.PollTimeout)
		case <-ticker.C:
		}
	}
}

func (r Runner) verifyRemovals(removals []optimizer.Removal, current []qbittorrent.Torrent, targetPath string) error {
	now := r.Now()
	for _, removal := range removals {
		torrent := findHash(current, removal.Hash)
		if torrent == nil {
			return fmt.Errorf("planned removal %s disappeared from qBittorrent", removal.Hash)
		}
		if torrent.Size != removal.Size || torrent.Progress != 1 || torrent.Category != r.Config.QBittorrent.Category || !torrent.AutoTMM ||
			!within(targetPath, torrent.SavePath) ||
			torrent.UPRate > r.Config.Policy.ActiveUploadRate || now.Sub(torrent.AddedOn) < r.Config.Policy.MinimumResidency ||
			(!torrent.LastActivity.IsZero() && now.Sub(torrent.LastActivity) < r.Config.Policy.MinimumIdle) || busyState(torrent.State) {
			return fmt.Errorf("planned removal %s changed and is no longer safe to delete", removal.Hash)
		}
	}
	return nil
}

func emptyStoppedDownload(torrent qbittorrent.Torrent) bool {
	state := strings.ToLower(torrent.State)
	return torrent.Progress == 0 && torrent.AmountLeft == torrent.Size && (state == "stoppeddl" || state == "pauseddl")
}

func busyState(state string) bool {
	state = strings.ToLower(state)
	return strings.Contains(state, "downloading") || strings.Contains(state, "metadl") || strings.Contains(state, "checking") || strings.Contains(state, "moving") || strings.Contains(state, "allocating")
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
	root, windowsRoot := cleanRemotePath(root)
	child, windowsChild := cleanRemotePath(child)
	if root == "" || child == "" || windowsRoot != windowsChild {
		return false
	}
	if windowsRoot {
		root, child = strings.ToLower(root), strings.ToLower(child)
	}
	if root == "/" {
		return true
	}
	root = strings.TrimSuffix(root, "/")
	return child == root || strings.HasPrefix(child, root+"/")
}

// remotePathIsAbs reports whether a qBittorrent path is absolute on either
// supported qBittorrent host family. The client can run on a different OS than
// qBittorrent, so filepath.IsAbs would apply the wrong platform's rules.
func remotePathIsAbs(value string) bool {
	cleaned, _ := cleanRemotePath(value)
	return cleaned != ""
}

// cleanRemotePath normalizes an absolute qBittorrent save path without using
// the client OS. qBittorrent may be remote, including a Windows host managed
// from a Unix client or a Unix host managed from a Windows client.
func cleanRemotePath(value string) (string, bool) {
	if value == "" {
		return "", false
	}
	if len(value) >= 3 && isDriveLetter(value[0]) && value[1] == ':' && (value[2] == '/' || value[2] == '\\') {
		return strings.ToUpper(value[:1]) + ":" + path.Clean("/"+strings.ReplaceAll(value[2:], "\\", "/")), true
	}
	if strings.HasPrefix(value, `\\`) || strings.HasPrefix(value, "//") {
		cleaned := path.Clean("/" + strings.TrimLeft(strings.ReplaceAll(value, "\\", "/"), "/"))
		parts := strings.Split(strings.TrimPrefix(cleaned, "/"), "/")
		if len(parts) < 2 || parts[0] == "." || parts[1] == "." {
			return "", true
		}
		return "//" + strings.Join(parts, "/"), true
	}
	if strings.HasPrefix(value, "/") {
		return path.Clean(value), false
	}
	return "", false
}

func isDriveLetter(value byte) bool {
	return value >= 'a' && value <= 'z' || value >= 'A' && value <= 'Z'
}
