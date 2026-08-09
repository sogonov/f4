package fishplus

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// MaxJobPaths bounds how many path lines one job request carries. The helper
// refuses more, and the limit is repeated here so a caller finds out before
// half a request is on the wire.
//
// 32 leaves ample room for a ffind job: one directory + up to 30 masks + one
// content pattern. scan/hash/exec all use two or fewer paths.
const MaxJobPaths = 32

// DefaultPollLines is how many output lines one poll brings back. A poll is
// a round trip, so it should be worth making; a job producing more than this
// is drained by polling again immediately rather than by a bigger buffer.
const DefaultPollLines = 200

// jobPollMin and jobPollMax bound the interval between two polls of a job
// that has nothing new to say. The first polls come quickly because a short
// job should not be reported late, and the interval then grows so that a
// walk over a large disk does not cost a round trip per screen refresh.
const (
	jobPollMin = 60 * time.Millisecond
	jobPollMax = 750 * time.Millisecond
)

// jobDropTimeout bounds the cleanup request sent after a scan was abandoned.
// It runs on a context of its own, because the caller's is usually the one
// that was just cancelled.
const jobDropTimeout = 5 * time.Second

// ErrNoJobs is returned when the remote host cannot run background jobs at
// all, either because it has nowhere to put their output or because it is
// missing the tools that read it back.
var ErrNoJobs = errors.New("fishplus: the remote host cannot run background jobs")

// ErrJobKilled is returned when a job stopped because someone killed it.
var ErrJobKilled = errors.New("fishplus: the remote job was killed")

// JobState is what the helper reports about a job in every poll.
type JobState string

const (
	JobRunning JobState = "run"
	JobDone    JobState = "done"
	JobKilled  JobState = "kill"
)

// JobStatus is one poll of a job: where it stands, and whatever it printed
// since the previous poll.
type JobStatus struct {
	State JobState
	// Exit is the exit status of a finished job, or -1 when it is not
	// known yet.
	Exit int
	// Msg carries the first line the job wrote to its standard error, and
	// only when it failed.
	Msg string
	// Lines is the output produced since the last poll, whole lines only.
	Lines []string
	// More says the poll hit its limit, so the next one should follow
	// without waiting.
	More bool
}

// Finished reports whether the job will produce nothing more.
func (s JobStatus) Finished() bool { return s.State != JobRunning }

// JobInfo is one entry of the remote job list.
type JobInfo struct {
	ID    int
	State JobState
	Kind  string
}

// CanRunJobs reports whether the remote host announced the background job
// machinery. A host without it is not an error: the caller does the work
// itself, one round trip at a time, exactly as it did before.
func (c *Client) CanRunJobs() bool { return c.sess.Features().Has("jobs") }

// StartJob starts a detached job on the remote host and returns its id. It
// does not wait for anything: the whole point of a job is that the session
// stays free while it runs.
func (c *Client) StartJob(ctx context.Context, kind string, paths []string, args ...string) (int, error) {
	if !c.CanRunJobs() {
		return 0, ErrNoJobs
	}
	if len(paths) > MaxJobPaths {
		return 0, fmt.Errorf("fishplus: job %q takes at most %d paths, got %d", kind, MaxJobPaths, len(paths))
	}
	full := append([]string{kind, strconv.Itoa(len(paths))}, args...)
	resp, err := c.sess.ExecPaths(ctx, "jstart", paths, full...)
	if err != nil {
		return 0, err
	}
	if err := resp.Err("jstart " + kind); err != nil {
		return 0, err
	}
	for _, line := range resp.Lines {
		if !strings.HasPrefix(line, "J ") {
			continue
		}
		id, convErr := strconv.Atoi(strings.TrimSpace(line[2:]))
		if convErr != nil || id < 1 {
			return 0, fmt.Errorf("fishplus: bad job id %q", line)
		}
		return id, nil
	}
	return 0, errors.New("fishplus: the remote host started a job without naming it")
}

// PollJob asks what a job is doing and collects at most limit of the lines
// it produced since the previous poll.
func (c *Client) PollJob(ctx context.Context, id, limit int) (JobStatus, error) {
	if limit <= 0 {
		limit = DefaultPollLines
	}
	resp, err := c.sess.Exec(ctx, "jpoll", strconv.Itoa(id), strconv.Itoa(limit))
	if err != nil {
		return JobStatus{}, err
	}
	if err := resp.Err("jpoll " + strconv.Itoa(id)); err != nil {
		return JobStatus{}, err
	}
	if len(resp.Lines) == 0 {
		return JobStatus{}, errors.New("fishplus: a job poll came back without a status line")
	}
	st, err := parseJobStatus(resp.Lines[0])
	if err != nil {
		return JobStatus{}, err
	}
	st.Lines = resp.Lines[1:]
	st.More = len(st.Lines) >= limit
	return st, nil
}

// KillJob stops a job but keeps whatever it produced, so a caller can still
// read the part of the answer that was ready.
func (c *Client) KillJob(ctx context.Context, id int) error {
	resp, err := c.sess.Exec(ctx, "jkill", strconv.Itoa(id))
	if err != nil {
		return err
	}
	return resp.Err("jkill " + strconv.Itoa(id))
}

// DropJob kills a job if it is still running and removes every trace of it
// from the remote host. Dropping a job that is not there succeeds: a client
// that cancelled something must not have to care whether it raced anything.
func (c *Client) DropJob(ctx context.Context, id int) error {
	resp, err := c.sess.Exec(ctx, "jdrop", strconv.Itoa(id))
	if err != nil {
		return err
	}
	return resp.Err("jdrop " + strconv.Itoa(id))
}

// ListJobs returns the jobs this session has started and not yet dropped.
func (c *Client) ListJobs(ctx context.Context) ([]JobInfo, error) {
	resp, err := c.sess.Exec(ctx, "jlist")
	if err != nil {
		return nil, err
	}
	if err := resp.Err("jlist"); err != nil {
		return nil, err
	}
	jobs := make([]JobInfo, 0, len(resp.Lines))
	for _, line := range resp.Lines {
		f := strings.SplitN(strings.TrimSpace(line), " ", 3)
		if len(f) < 2 {
			continue
		}
		id, convErr := strconv.Atoi(f[0])
		if convErr != nil {
			continue
		}
		info := JobInfo{ID: id, State: JobState(f[1])}
		if len(f) == 3 {
			info.Kind = f[2]
		}
		jobs = append(jobs, info)
	}
	return jobs, nil
}

// parseJobStatus reads the "S <state> <exit|-> [message]" line every poll
// begins with.
func parseJobStatus(line string) (JobStatus, error) {
	if !strings.HasPrefix(line, "S ") {
		return JobStatus{}, fmt.Errorf("fishplus: expected a job status line, got %q", line)
	}
	f := strings.SplitN(strings.TrimSpace(line[2:]), " ", 3)
	if len(f) < 2 {
		return JobStatus{}, fmt.Errorf("fishplus: bad job status line %q", line)
	}
	st := JobStatus{State: JobState(f[0]), Exit: -1}
	switch st.State {
	case JobRunning, JobDone, JobKilled:
	default:
		return JobStatus{}, fmt.Errorf("fishplus: unknown job state %q", f[0])
	}
	if f[1] != "-" {
		code, err := strconv.Atoi(f[1])
		if err != nil {
			return JobStatus{}, fmt.Errorf("fishplus: bad job exit status %q", f[1])
		}
		st.Exit = code
	}
	if len(f) == 3 {
		st.Msg = strings.TrimSpace(f[2])
	}
	return st, nil
}

// ScanStats is what a walk over a remote tree found. The fields are the ones
// f4's own scanner keeps, so a caller can hand them straight to the copier
// or to the size display.
type ScanStats struct {
	Bytes    int64
	DirBytes int64
	Files    int64
	Dirs     int64
}

// ScanProgress is an intermediate report of a running scan: the totals so
// far, and the entry the remote walk had reached when it printed them.
type ScanProgress struct {
	ScanStats
	Path string
}

// CanScan reports whether the remote host can walk a tree for us. It needs
// the job machinery, a find to do the walking, an awk to do the summing and
// any of the listing backends to supply the sizes.
func (c *Client) CanScan() bool {
	feats := c.sess.Features()
	return c.CanRunJobs() && feats.Has("findbin") && feats.Has("awk") && feats.ListingMode() != ""
}

// StartScan starts a tree scan and returns the job id. The caller is
// responsible for the job from then on: FollowScan reads it, DropJob
// forgets it.
func (c *Client) StartScan(ctx context.Context, dir string) (int, error) {
	if !c.CanScan() {
		return 0, ErrNoJobs
	}
	return c.StartJob(ctx, "scan", []string{dir})
}

// Scan walks a whole remote tree and returns what it found, reporting
// progress through cb on the way. A cancelled scan is cancelled on the
// remote host as well: work the user stopped watching must not keep
// somebody's server busy with nothing to show for it. A caller that wants
// the opposite — a job that outlives the dialog it was started from — builds
// it out of StartScan and FollowScan, which never kill anything.
func (c *Client) Scan(ctx context.Context, dir string, cb func(ScanProgress)) (ScanStats, error) {
	id, err := c.StartScan(ctx, dir)
	if err != nil {
		return ScanStats{}, err
	}
	stats, err := c.FollowScan(ctx, id, cb)
	c.dropJobQuietly(id)
	return stats, err
}

// FollowScan polls a scan job until it ends. The polls themselves run on a
// context of their own: a request that is cancelled while its answer is on
// the wire desynchronizes the session, so cancellation is noticed between
// two polls instead, which costs one round trip and keeps the session
// usable — which is the entire reason for doing this as a job.
func (c *Client) FollowScan(ctx context.Context, id int, cb func(ScanProgress)) (ScanStats, error) {
	reqCtx := context.WithoutCancel(ctx)
	var (
		stats    ScanStats
		gotTotal bool
		wait     = jobPollMin
	)
	for {
		st, err := c.PollJob(reqCtx, id, DefaultPollLines)
		if err != nil {
			return stats, err
		}
		fresh := false
		for _, line := range st.Lines {
			p, total, parseErr := parseScanLine(line)
			if parseErr != nil {
				// A remote tool that printed something of its own must
				// not cost the caller the numbers that did parse.
				continue
			}
			stats = p.ScanStats
			if total {
				gotTotal = true
				continue
			}
			fresh = true
			if cb != nil {
				cb(p)
			}
		}
		if err := ctx.Err(); err != nil {
			return stats, err
		}
		if st.More {
			continue
		}
		switch st.State {
		case JobKilled:
			return stats, ErrJobKilled
		case JobDone:
			if st.Exit != 0 {
				msg := st.Msg
				if msg == "" {
					msg = "the remote scan failed with status " + strconv.Itoa(st.Exit)
				}
				return stats, &RemoteError{Cmd: "scan", Msg: msg}
			}
			if !gotTotal {
				return stats, errors.New("fishplus: the remote scan ended without a total")
			}
			return stats, nil
		}
		if fresh {
			wait = jobPollMin
		}
		if err := sleepCtx(ctx, wait); err != nil {
			return stats, err
		}
		wait *= 2
		if wait > jobPollMax {
			wait = jobPollMax
		}
	}
}

// dropJobQuietly frees a job on a context the caller cannot have cancelled.
// Failing to clean up is not worth an error of its own: the session drops
// the whole job directory when it ends anyway.
func (c *Client) dropJobQuietly(id int) {
	if c.sess.Broken() {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), jobDropTimeout)
	defer cancel()
	c.DropJob(ctx, id)
}

// parseScanLine reads one line of scan output: "P <bytes> <dirbytes>
// <files> <dirs> <path>" while the walk runs, "T <bytes> <dirbytes> <files>
// <dirs>" at the end. The path comes last because it is the only field that
// can contain a space.
func parseScanLine(line string) (ScanProgress, bool, error) {
	var p ScanProgress
	if len(line) < 2 || line[1] != ' ' || (line[0] != 'P' && line[0] != 'T') {
		return p, false, fmt.Errorf("fishplus: unexpected scan line %q", line)
	}
	total := line[0] == 'T'
	f := strings.SplitN(line[2:], " ", 5)
	if len(f) < 4 {
		return p, false, fmt.Errorf("fishplus: bad scan line %q", line)
	}
	var nums [4]int64
	for i := 0; i < 4; i++ {
		v, err := strconv.ParseInt(f[i], 10, 64)
		if err != nil {
			return p, false, fmt.Errorf("fishplus: bad scan line %q", line)
		}
		nums[i] = v
	}
	p.Bytes, p.DirBytes, p.Files, p.Dirs = nums[0], nums[1], nums[2], nums[3]
	if len(f) == 5 {
		p.Path = f[4]
	}
	return p, total, nil
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
