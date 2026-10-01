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
	defaultPollInterval = time.Second
	// Initialization and physical deletion can outlast the HTTP acknowledgement,
	// especially while a busy disk stalls qBittorrent's Web API.
	defaultPollTimeout = 10 * time.Minute
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
	mutations    *[]Mutation
}

type Report struct {
	Mode               string             `json:"mode"`
	GeneratedAt        time.Time          `json:"generated_at"`
	ConfigPath         string             `json:"config_path"`
	DownloadPath       string             `json:"download_path"`
	Budget             budget.Result      `json:"budget"`
	TorrentCount       int                `json:"torrent_count"`
	CandidateCount     int                `json:"candidate_count"`
	ProjectedUsedBytes int64              `json:"projected_used_bytes"`
	PlanningHorizon    string             `json:"planning_horizon"`
	ReplacementMargin  float64            `json:"replacement_margin"`
	NetGain            float64            `json:"net_gain_score_bytes"`
	Recoveries         []Recovery         `json:"recoveries,omitempty"`
	Actions            []Action           `json:"actions"`
	SkippedCandidates  []SkippedCandidate `json:"skipped_candidates,omitempty"`
	Mutations          []Mutation         `json:"mutations,omitempty"`
	Replans            int                `json:"replans,omitempty"`
	Error              string             `json:"error,omitempty"`
}

// Mutation records an API acknowledgement, not proof that asynchronous work
// has finished. An unconfirmed request may or may not have reached qBittorrent.
type Mutation struct {
	Operation   string   `json:"operation"`
	Hashes      []string `json:"hashes"`
	Status      string   `json:"status"`
	DeleteFiles *bool    `json:"delete_files,omitempty"`
}

type Recovery struct {
	Action string `json:"action"`
	Hash   string `json:"hash"`
	Name   string `json:"name"`
}

type SkippedCandidate struct {
	CandidateID string `json:"candidate_id"`
	Reason      string `json:"reason"`
}

type Action struct {
	CandidateID      string     `json:"candidate_id"`
	Name             string     `json:"name"`
	SizeBytes        int64      `json:"size_bytes"`
	Seeders          int        `json:"seeders"`
	Leechers         int        `json:"leechers"`
	Opportunity      float64    `json:"opportunity"`
	UploadMultiplier int        `json:"upload_multiplier"`
	UploadScore      float64    `json:"upload_score_bytes"`
	FreeUntil        *time.Time `json:"free_until"` // Nil means no scheduled end.
	Removals         []Removal  `json:"removals"`
	Applied          bool       `json:"applied"`
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

func (r Runner) Execute(ctx context.Context, apply bool) (report Report, runErr error) {
	var resolved *candidateResolver
	defer func() {
		if resolved != nil {
			report.SkippedCandidates = resolved.skipped
		}
		if runErr != nil {
			report.Error = runErr.Error()
		}
	}()
	r.mutations = &report.Mutations
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
	report = Report{Mode: "plan", GeneratedAt: now, ConfigPath: r.Config.Path, PlanningHorizon: r.Config.Policy.PlanningHorizon.String(), ReplacementMargin: r.Config.Policy.ReplacementMargin, Actions: []Action{}}
	if apply {
		report.Mode = "apply"
	}
	state, err := r.snapshot(ctx)
	if err != nil {
		return report, err
	}
	report.DownloadPath = state.targetPath
	report.Budget = state.budget
	report.TorrentCount = len(state.all)
	report.ProjectedUsedBytes = state.budget.UsedBytes
	results, err := r.MTeam.Search(ctx)
	if err != nil {
		return report, fmt.Errorf("search M-Team freeleech torrents: %w", err)
	}
	report.CandidateCount = len(results)
	candidates, err := optimizerCandidates(results)
	if err != nil {
		return report, err
	}

	resolved = newCandidateResolver(r.MTeam)
	recoveries, changed, err := r.recoverPending(ctx, state, candidates, apply, resolved)
	report.Recoveries = recoveries
	if err != nil {
		return report, err
	}
	if changed {
		state, err = r.snapshot(ctx)
		if err != nil {
			return report, fmt.Errorf("refresh qBittorrent after pending recovery: %w", err)
		}
	}
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

	// Recovery can wait for qBittorrent, so judge offer eligibility at the
	// time of planning rather than at the start of the run.
	plan, err := r.buildPlan(ctx, r.Now(), candidates, state, resolved)
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

	if err := r.applyPlan(ctx, candidates, plan, resolved, &report); err != nil {
		return report, err
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
		if r.isRecoverablePending(torrent, state.targetPath) {
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
	var validCandidates []optimizer.Candidate
	var stale []qbittorrent.Torrent
	for _, torrent := range pending {
		candidate, err := r.pendingCandidate(ctx, torrent, candidates, resolved)
		if err != nil {
			return recoveries, false, err
		}
		if candidate != nil {
			valid = append(valid, torrent)
			validCandidates = append(validCandidates, *candidate)
			continue
		}
		stale = append(stale, torrent)
	}
	if _, err := r.verifyRecoverablePending(ctx, pending, state.targetPath); err != nil {
		return recoveries, false, err
	}
	if len(stale) > 0 {
		hashes := torrentHashes(stale)
		if err := r.delete(ctx, hashes, false); err != nil {
			return nil, false, fmt.Errorf("remove unverifiable pending torrents: %w", err)
		}
		if err := r.waitForRegistrationsRemoved(ctx, hashes); err != nil {
			return recoveries, true, fmt.Errorf("confirm unverifiable pending torrents removed: %w", err)
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
	if state.budget.UsedBytes > state.budget.LimitBytes {
		fitted, err := r.waitForPendingBudget(ctx)
		var timeout *pollTimeoutError
		switch {
		case err == nil:
			state = fitted
		case errors.As(err, &timeout) && timeout.pending != nil:
			// The budget was observed and stayed short; remove the registrations below.
		default:
			return recoveries, len(stale) > 0, fmt.Errorf("refresh qBittorrent before judging pending torrents over budget: %w", err)
		}
	}
	var freeUntil time.Time
	if state.budget.UsedBytes <= state.budget.LimitBytes {
		freeUntil, err = r.verifyFreeleech(ctx, validCandidates)
		if err != nil {
			return recoveries, len(stale) > 0, fmt.Errorf("verify freeleech before resuming pending torrents: %w", err)
		}
	}
	if state.budget.UsedBytes <= state.budget.LimitBytes {
		hashes, err := r.verifyRecoverablePendingForStart(ctx, valid, state.targetPath)
		if err != nil {
			return recoveries, len(stale) > 0, err
		}
		if !r.hasFreeleechTime(freeUntil) {
			return recoveries, len(stale) > 0, errors.New("pending torrents no longer have the required freeleech time")
		}
		if err := r.start(ctx, hashes); err != nil {
			return recoveries, len(stale) > 0, fmt.Errorf("resume pending torrents: %w", err)
		}
		if err := r.waitForStarted(ctx, valid, state.targetPath); err != nil {
			return recoveries, true, fmt.Errorf("confirm pending torrents resumed: %w", err)
		}
		for _, torrent := range valid {
			recoveries = append(recoveries, Recovery{Action: "resume", Hash: torrent.Hash, Name: torrent.Name})
		}
		return recoveries, true, nil
	}
	hashes, err := r.verifyRecoverablePending(ctx, valid, state.targetPath)
	if err != nil {
		return recoveries, len(stale) > 0, err
	}
	if err := r.delete(ctx, hashes, false); err != nil {
		return recoveries, len(stale) > 0, fmt.Errorf("remove over-budget pending torrents: %w", err)
	}
	if err := r.waitForRegistrationsRemoved(ctx, hashes); err != nil {
		return recoveries, true, fmt.Errorf("confirm over-budget pending torrents removed: %w", err)
	}
	for _, torrent := range valid {
		recoveries = append(recoveries, Recovery{Action: "remove", Hash: torrent.Hash, Name: torrent.Name})
	}
	return recoveries, true, nil
}

func (r Runner) verifyRecoverablePending(ctx context.Context, expected []qbittorrent.Torrent, targetPath string) ([]string, error) {
	current, err := r.QBittorrent.Torrents(ctx)
	if err != nil {
		return nil, fmt.Errorf("refresh recoverable qBittorrent torrents before mutation: %w", err)
	}
	return r.verifyRecoverablePendingTorrents(current, expected, targetPath)
}

// A stopped torrent with zero verified progress may still refer to files used
// by another torrent. All starts require a fresh budget and isolated content.
func (r Runner) snapshotForStart(ctx context.Context, expected []qbittorrent.Torrent, targetPath string) (snapshot, error) {
	state, err := r.snapshot(ctx)
	if err != nil {
		return snapshot{}, fmt.Errorf("refresh pending qBittorrent torrents before start: %w", err)
	}
	if !within(targetPath, state.targetPath) || !within(state.targetPath, targetPath) {
		return snapshot{}, errors.New("qBittorrent category save path changed before start")
	}
	if err := validatePlannedUsed(state, 0, nil); err != nil {
		return snapshot{}, fmt.Errorf("pending torrents no longer fit the current budget before start: %w", err)
	}
	if state.budget.FreeBytes-state.budget.OutstandingBytes < state.budget.RequiredFreeBytes {
		return snapshot{}, errors.New("pending torrents cannot preserve the disk free-space reserve before start")
	}
	if err := verifyContentIsolation(state.all, torrentHashes(expected)); err != nil {
		return snapshot{}, err
	}
	return state, nil
}

func (r Runner) verifyAddedPayloadForStart(ctx context.Context, expected []qbittorrent.Torrent, targetPath string) ([]string, error) {
	state, err := r.snapshotForStart(ctx, expected, targetPath)
	if err != nil {
		return nil, err
	}
	return r.verifyAddedPayloadTorrents(state.all, expected, targetPath)
}

func (r Runner) verifyRecoverablePendingForStart(ctx context.Context, expected []qbittorrent.Torrent, targetPath string) ([]string, error) {
	state, err := r.snapshotForStart(ctx, expected, targetPath)
	if err != nil {
		return nil, err
	}
	return r.verifyRecoverablePendingTorrents(state.all, expected, targetPath)
}

func (r Runner) verifyAddedPayloadTorrents(current, expected []qbittorrent.Torrent, targetPath string) ([]string, error) {
	for _, expectedTorrent := range expected {
		torrent := findHash(current, expectedTorrent.Hash)
		if torrent == nil {
			return nil, fmt.Errorf("added torrent %q disappeared before mutation", expectedTorrent.Hash)
		}
		if torrent.Size != expectedTorrent.Size || !r.isAddedPayload(*torrent, targetPath) {
			return nil, fmt.Errorf("added torrent %q changed and is no longer safe to mutate", expectedTorrent.Hash)
		}
	}
	return torrentHashes(expected), nil
}

func (r Runner) verifyRecoverablePendingTorrents(current, expected []qbittorrent.Torrent, targetPath string) ([]string, error) {
	for _, expectedTorrent := range expected {
		torrent := findHash(current, expectedTorrent.Hash)
		if torrent == nil {
			return nil, fmt.Errorf("recoverable pending torrent %q disappeared before mutation", expectedTorrent.Hash)
		}
		if torrent.Size != expectedTorrent.Size || !r.isRecoverablePending(*torrent, targetPath) {
			return nil, fmt.Errorf("recoverable pending torrent %q changed and is no longer safe to mutate", expectedTorrent.Hash)
		}
	}
	return torrentHashes(expected), nil
}

func (r Runner) isPending(torrent qbittorrent.Torrent, targetPath string) bool {
	return torrent.Category == r.Config.QBittorrent.Category && torrent.AutoTMM &&
		within(targetPath, torrent.SavePath) && emptyStoppedDownload(torrent)
}

func (r Runner) isRecoverablePending(torrent qbittorrent.Torrent, targetPath string) bool {
	return torrent.Category == r.Config.QBittorrent.Category && torrent.AutoTMM &&
		within(targetPath, torrent.SavePath) && stoppedIncompleteDownload(torrent)
}

func (r Runner) isAddedPayload(torrent qbittorrent.Torrent, targetPath string) bool {
	return torrent.Category == r.Config.QBittorrent.Category && torrent.AutoTMM &&
		within(targetPath, torrent.SavePath) && stoppedPayload(torrent)
}

func torrentHashes(torrents []qbittorrent.Torrent) []string {
	hashes := make([]string, len(torrents))
	for index, torrent := range torrents {
		hashes[index] = torrent.Hash
	}
	return hashes
}

func (r Runner) pendingCandidate(ctx context.Context, torrent qbittorrent.Torrent, candidates []optimizer.Candidate, resolved *candidateResolver) (*optimizer.Candidate, error) {
	for _, candidate := range candidates {
		if candidate.Size != torrent.Size ||
			!r.hasFreeleechTime(candidate.FreeUntil) {
			continue
		}
		info, err := resolved.resolve(ctx, candidate.ID, candidate.Size)
		if err != nil {
			// The daily quota is the one known per-torrent refusal. Any other
			// failure may be transient and may concern this torrent's own offer,
			// so stop instead of judging a valid pending torrent stale.
			if errors.Is(err, mteam.ErrTorrentDownloadLimit) {
				continue
			}
			return nil, fmt.Errorf("recover pending torrent %s: %w", torrent.Hash, err)
		}
		if strings.EqualFold(info.hash, torrent.Hash) {
			return &candidate, nil
		}
	}
	return nil, nil
}

func (r Runner) hasFreeleechTime(until time.Time) bool {
	now := r.Now()
	return until.IsZero() || (until.After(now) && until.Sub(now) >= r.Config.Policy.MinimumFreeleechRemaining)
}

// verifyFreeleech requires fresh evidence from the current freeleech feed.
// Offers absent from the configured search pages cannot authorize a download.
// It returns the earliest scheduled expiry, or zero if every offer has no end,
// so callers can recheck time after qBittorrent state validation, immediately
// before mutation.
func (r Runner) verifyFreeleech(ctx context.Context, expected []optimizer.Candidate) (time.Time, error) {
	results, err := r.MTeam.Search(ctx)
	if err != nil {
		return time.Time{}, fmt.Errorf("refresh M-Team freeleech torrents: %w", err)
	}
	candidates, err := optimizerCandidates(results)
	if err != nil {
		return time.Time{}, err
	}
	current := make(map[string]optimizer.Candidate, len(candidates))
	for _, candidate := range candidates {
		current[candidate.ID] = candidate
	}
	var earliest time.Time
	for _, candidate := range expected {
		fresh, ok := current[candidate.ID]
		if !ok {
			return time.Time{}, fmt.Errorf("candidate %s no longer has a verifiable freeleech offer", candidate.ID)
		}
		if fresh.Size != candidate.Size {
			return time.Time{}, fmt.Errorf("candidate %s changed size in the current freeleech offer", candidate.ID)
		}
		if !r.hasFreeleechTime(fresh.FreeUntil) {
			return time.Time{}, fmt.Errorf("candidate %s no longer has the required freeleech time", candidate.ID)
		}
		if !fresh.FreeUntil.IsZero() && (earliest.IsZero() || fresh.FreeUntil.Before(earliest)) {
			earliest = fresh.FreeUntil
		}
	}
	return earliest, nil
}

func optimizerCandidates(results []mteam.Torrent) ([]optimizer.Candidate, error) {
	candidates := make([]optimizer.Candidate, 0, len(results))
	for _, result := range results {
		if result.ID <= 0 || result.Seeders < 0 || result.Leechers < 0 || result.Seeders > math.MaxInt || result.Leechers > math.MaxInt {
			return nil, fmt.Errorf("M-Team returned invalid candidate %d", result.ID)
		}
		multiplier, err := uploadMultiplier(result.Discount)
		if err != nil {
			return nil, err
		}
		candidates = append(candidates, optimizer.Candidate{
			ID: strconv.FormatInt(result.ID, 10), Name: result.Name, Size: result.Size,
			Seeders: int(result.Seeders), Leechers: int(result.Leechers),
			PublishedAt: result.PublishedAt, FreeUntil: result.DiscountEndTime,
			UploadMultiplier: multiplier,
		})
	}
	return candidates, nil
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
			Removals: make([]Removal, 0, len(addition.Removals)),
		}
		if !candidate.FreeUntil.IsZero() {
			action.FreeUntil = &candidate.FreeUntil
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
	if !r.hasFreeleechTime(candidate.FreeUntil) {
		return fmt.Errorf("candidate %s no longer has the required freeleech time", candidate.ID)
	}
	info, err := resolved.resolve(ctx, candidate.ID, candidate.Size)
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
	if preallocate {
		available := beforeAdd.budget.FreeBytes - beforeAdd.budget.RequiredFreeBytes
		if candidate.Size > available || beforeAdd.budget.OutstandingBytes > available-candidate.Size {
			return fmt.Errorf("candidate %s cannot be safely preallocated while preserving %d free bytes and outstanding downloads", candidate.ID, beforeAdd.budget.RequiredFreeBytes)
		}
	}
	if findHash(beforeAdd.all, hash) != nil {
		return fmt.Errorf("candidate %s already exists in qBittorrent as %s", candidate.ID, hash)
	}
	if err := r.verifyRemovals(addition.Removals, beforeAdd.all, beforeAdd.targetPath); err != nil {
		return err
	}
	err = r.QBittorrent.Add(ctx, qbittorrent.AddRequest{
		Metainfo: info.bytes, MetainfoName: candidate.ID + ".torrent",
		SavePath: beforeAdd.targetPath, Category: r.Config.QBittorrent.Category,
		Stopped: true, AutoTMM: true,
	})
	r.recordMutation("add", []string{hash}, err)
	if err != nil {
		return fmt.Errorf("add M-Team torrent %s to qBittorrent: %w", candidate.ID, err)
	}
	rollback := func(cause error) error {
		return r.rollbackPending(ctx, hash, beforeAdd.targetPath, candidate.ID, cause)
	}
	added, _, err := r.waitForTorrent(ctx, hash)
	if err != nil {
		return rollback(fmt.Errorf("verify added M-Team torrent %s: %w", candidate.ID, err))
	}
	if added.Size != candidate.Size || !r.isAddedPayload(*added, beforeAdd.targetPath) {
		return rollback(fmt.Errorf("added M-Team torrent %s does not match its stopped, automatically managed category plan: state=%q size=%d (expected %d) amount_left=%d progress=%g category=%q auto_tmm=%t save_path=%q", candidate.ID, added.State, added.Size, candidate.Size, added.AmountLeft, added.Progress, added.Category, added.AutoTMM, added.SavePath))
	}
	var freeUntil time.Time
	if len(addition.Removals) > 0 {
		freeUntil, err = r.verifyFreeleech(ctx, []optimizer.Candidate{candidate})
		if err != nil {
			return rollback(err)
		}
	}
	afterAdd, err := r.snapshot(ctx)
	if err != nil {
		return rollback(fmt.Errorf("refresh qBittorrent after adding candidate %s: %w", candidate.ID, err))
	}
	expected := []qbittorrent.Torrent{{Hash: hash, Size: candidate.Size}}
	if _, err := r.verifyAddedPayloadTorrents(afterAdd.all, expected, afterAdd.targetPath); err != nil {
		return rollback(fmt.Errorf("verify candidate %s before replacement: %w", candidate.ID, err))
	}
	if err := verifyContentIsolation(afterAdd.all, []string{hash}); err != nil {
		return rollback(fmt.Errorf("verify candidate %s content before replacement: %w", candidate.ID, err))
	}
	if err := validatePlannedUsed(afterAdd, 0, addition.Removals); err != nil {
		return rollback(fmt.Errorf("candidate %s no longer fits the current budget: %w", candidate.ID, err))
	}
	if err := r.verifyRemovals(addition.Removals, afterAdd.all, afterAdd.targetPath); err != nil {
		return rollback(err)
	}
	if len(addition.Removals) > 0 {
		if !r.hasFreeleechTime(freeUntil) {
			return rollback(fmt.Errorf("candidate %s no longer has the required freeleech time", candidate.ID))
		}
		hashes := make([]string, len(addition.Removals))
		for index, removal := range addition.Removals {
			hashes[index] = removal.Hash
		}
		if err := r.delete(ctx, hashes, true); err != nil {
			return fmt.Errorf("delete replaced torrents for candidate %s: %w", candidate.ID, err)
		}
	}
	var afterDelete snapshot
	if len(addition.Removals) > 0 {
		afterDelete, err = r.waitForRemovals(ctx, addition.Removals)
	} else {
		afterDelete, err = r.snapshot(ctx)
	}
	if err != nil {
		return fmt.Errorf("refresh qBittorrent after deleting replacements for candidate %s: %w", candidate.ID, err)
	}
	if err := validatePlannedUsed(afterDelete, 0, nil); err != nil {
		return fmt.Errorf("candidate %s no longer fits the current budget after deletion: %w", candidate.ID, err)
	}
	freeUntil, err = r.verifyFreeleech(ctx, []optimizer.Candidate{candidate})
	if err != nil {
		return err
	}
	hashes, err := r.verifyAddedPayloadForStart(ctx, expected, afterDelete.targetPath)
	if err != nil {
		return fmt.Errorf("verify candidate %s before starting it: %w", candidate.ID, err)
	}
	if !r.hasFreeleechTime(freeUntil) {
		return fmt.Errorf("candidate %s no longer has the required freeleech time", candidate.ID)
	}
	if err := r.start(ctx, hashes); err != nil {
		return fmt.Errorf("start candidate %s: %w", candidate.ID, err)
	}
	if err := r.waitForStarted(ctx, expected, afterDelete.targetPath); err != nil {
		return fmt.Errorf("confirm candidate %s started: %w", candidate.ID, err)
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
	if err := r.delete(ctx, []string{hash}, false); err != nil {
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
		return &budgetExceededError{projected: projected, limit: state.budget.LimitBytes}
	}
	return nil
}

func (r Runner) waitForTorrent(ctx context.Context, hash string) (*qbittorrent.Torrent, []qbittorrent.Torrent, error) {
	var found *qbittorrent.Torrent
	var torrents []qbittorrent.Torrent
	err := r.poll(ctx, "wait for added torrent to initialize", func(ctx context.Context) (error, error) {
		var err error
		torrents, err = r.QBittorrent.Torrents(ctx)
		if err != nil {
			return nil, err
		}
		found = findHash(torrents, hash)
		if found == nil {
			return fmt.Errorf("torrent %s did not appear", hash), nil
		}
		switch strings.ToLower(found.State) {
		case "checkingresumedata", "checkingdl", "checkingup":
			// qBittorrent briefly checks resume data after a stopped add.
			// Wait for its stable state before enforcing Swarmfolio's plan.
			return fmt.Errorf("torrent %s did not leave transient checking state %q", hash, found.State), nil
		}
		return nil, nil
	})
	if err != nil {
		return nil, nil, err
	}
	return found, torrents, nil
}

func (r Runner) verifyRemovals(removals []optimizer.Removal, current []qbittorrent.Torrent, targetPath string) error {
	now := r.Now()
	hashes := make([]string, 0, len(removals))
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
		hashes = append(hashes, removal.Hash)
	}
	return verifyContentIsolation(current, hashes)
}

func emptyStoppedDownload(torrent qbittorrent.Torrent) bool {
	state := strings.ToLower(torrent.State)
	return torrent.Progress == 0 && torrent.AmountLeft == torrent.Size && (state == "stoppeddl" || state == "pauseddl")
}

func stoppedIncompleteDownload(torrent qbittorrent.Torrent) bool {
	state := strings.ToLower(torrent.State)
	return torrent.Progress >= 0 && torrent.Progress < 1 && torrent.AmountLeft > 0 && torrent.AmountLeft <= torrent.Size &&
		(state == "stoppeddl" || state == "pauseddl")
}

func stoppedPayload(torrent qbittorrent.Torrent) bool {
	state := strings.ToLower(torrent.State)
	if state == "stoppeddl" || state == "pauseddl" {
		return stoppedIncompleteDownload(torrent)
	}
	return (state == "stoppedup" || state == "pausedup") && torrent.Progress == 1 && torrent.AmountLeft == 0
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
