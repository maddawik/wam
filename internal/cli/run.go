package cli

import (
	"context"
	"fmt"
	"os"
	"sync"

	"github.com/maddawik/wam/internal/errs"
	"github.com/maddawik/wam/internal/install"
	"github.com/maddawik/wam/internal/lock"
	"github.com/maddawik/wam/internal/manifest"
)

// downloadLimit is how many addons download at once.
const downloadLimit = 4

// addonRun is one sync or update: the lock it changes, what it has done, and
// where it reports. Its methods run on the command's goroutine.
type addonRun struct {
	ctx       context.Context
	addonsDir string
	lockFile  string
	l         *lock.Lock
	prog      *progress

	changes []syncChange
	fails   []*errs.Error
}

// startRun loads the lock beside manifestPath and readies addonsDir, undoing
// whatever a crashed run left half done before anything changes. Set prog
// once the run knows how many addons it has.
func startRun(ctx context.Context, manifestPath, addonsDir string) (*addonRun, error) {
	lockFile := lockPath(manifestPath)
	l, err := lock.Load(lockFile)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(addonsDir, 0o755); err != nil {
		return nil, errs.Wrap(errs.Runtime, err, fmt.Sprintf("cannot create %s", addonsDir), "")
	}
	if err := install.Recover(addonsDir); err != nil {
		return nil, err
	}
	return &addonRun{
		ctx: ctx, addonsDir: addonsDir, lockFile: lockFile, l: l, changes: []syncChange{},
	}, nil
}

// finish runs body, then returns the run's changes and its error: body's,
// else the summary of its failures.
func (r *addonRun) finish(body func() error) ([]syncChange, error) {
	err := body()
	if err == nil {
		err = failure(r.fails, len(r.changes) > 0)
	}
	if cerr := r.prog.close(); err == nil {
		err = cerr
	}
	return r.changes, err
}

// ownerOf is the lock's owner of a folder, other than key.
func (r *addonRun) ownerOf(key string) install.OwnerFunc {
	return func(folder string) (string, bool) { return r.l.Owner(folder, key) }
}

// failed records that key failed, and carries on.
func (r *addonRun) failed(key string, err error) {
	r.fails = append(r.fails, addonError(key, err))
	r.prog.finish(key, "failed", "")
}

// changed reports c, records it, and saves the lock it changed.
func (r *addonRun) changed(c syncChange) error {
	r.changes = append(r.changes, c)
	r.prog.finish(c.Addon, c.summary(), c.line())
	return r.save()
}

func (r *addonRun) save() error {
	if err := lock.Save(r.lockFile, r.l); err != nil {
		return errs.Wrap(errs.Runtime, err, fmt.Sprintf("cannot write lock %s", r.lockFile), "")
	}
	return nil
}

// downloadJob is one addon to download and install.
type downloadJob struct {
	key string
	// fetch downloads the addon. It runs off the command's goroutine, at
	// the same time as other jobs' fetches, so it must not touch the run.
	fetch func(ctx context.Context) (download, error)
	// install installs a fetched download, updating the lock entry, and
	// returns what changed, or nil when nothing did. It runs on the
	// command's goroutine, one job at a time.
	install func(d download) (*syncChange, error)
	// unchanged, if set, is called instead of reporting "up to date".
	unchanged func()
}

// unsupportedJob fails a, whose source wam can't install from. run is the
// command to suggest.
func unsupportedJob(a manifest.Addon, run string) downloadJob {
	err := errs.New(errs.Usage, fmt.Sprintf("source %q is not supported; only github", a.Source), run)
	return downloadJob{key: a.Key, fetch: func(context.Context) (download, error) { return download{}, err }}
}

// downloadAll runs jobs, fetching up to downloadLimit at once. Each is
// installed as its download finishes, one at a time, and reported then. A
// job that fails is recorded and the rest carry on. The lock is saved after
// each change, so a run killed part way still records what it put in the
// AddOns folder; if saving fails, the run stops with that error. Changes and
// failures are recorded in job order, however the downloads finished.
func (r *addonRun) downloadAll(jobs []downloadJob) error {
	ctx, cancel := context.WithCancel(r.ctx)
	defer cancel()
	changes := make([]*syncChange, len(jobs))
	fails := make([]*errs.Error, len(jobs))
	var stop error
	fanOut(len(jobs), func(i int) func() {
		j := jobs[i]
		if ctx.Err() != nil {
			return func() { r.prog.finish(j.key, "stopped", "") }
		}
		r.prog.start(j.key)
		d, err := j.fetch(ctx)
		return func() {
			if err == nil {
				defer d.cleanup()
			}
			if stop != nil || r.ctx.Err() != nil {
				r.prog.finish(j.key, "stopped", "")
				return
			}
			var c *syncChange
			if err == nil {
				c, err = j.install(d)
			}
			switch {
			case err != nil:
				fails[i] = addonError(j.key, err)
				r.prog.finish(j.key, "failed", "")
			case c == nil:
				r.prog.finish(j.key, "up to date", "")
				if j.unchanged != nil {
					j.unchanged()
				}
			default:
				changes[i] = c
				r.prog.finish(j.key, c.summary(), c.line())
				if err := r.save(); err != nil {
					stop = err
					cancel()
				}
			}
		}
	})
	for i := range jobs {
		if changes[i] != nil {
			r.changes = append(r.changes, *changes[i])
		}
		if fails[i] != nil {
			r.fails = append(r.fails, fails[i])
		}
	}
	if stop != nil {
		return stop
	}
	return r.ctx.Err()
}

// fanOut calls work(i) for each i in [0, n), at most downloadLimit at once,
// and calls the func each returns on the calling goroutine as soon as that
// work is done, so whatever the returned funcs change needs no locking.
// work must not touch that state. A panic in work is raised again on the
// calling goroutine.
func fanOut(n int, work func(i int) func()) {
	results := make(chan func())
	slots := make(chan struct{}, downloadLimit)
	go func() {
		var wg sync.WaitGroup
		for i := range n {
			slots <- struct{}{}
			wg.Go(func() {
				var done func()
				func() {
					defer func() {
						if r := recover(); r != nil {
							done = func() { panic(r) }
						}
					}()
					done = work(i)
				}()
				<-slots
				results <- done
			})
		}
		wg.Wait()
		close(results)
	}()
	// If a done func panics, let the workers finish rather than block.
	defer func() {
		for range results {
		}
	}()
	for done := range results {
		done()
	}
}
