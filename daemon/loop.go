package daemon

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/go-kit/kit/log"
	fluxmetrics "github.com/weaveworks/flux/metrics"
)

type LoopVars struct {
	SyncInterval         time.Duration
	RegistryPollInterval time.Duration

	initOnce       sync.Once
	syncSoon       chan struct{}
	pollImagesSoon chan struct{}
}

func (loop *LoopVars) ensureInit() {
	loop.initOnce.Do(func() {
		loop.syncSoon = make(chan struct{}, 1)
		loop.pollImagesSoon = make(chan struct{}, 1)
	})
}

func (d *Daemon) Loop(stop chan struct{}, wg *sync.WaitGroup, logger log.Logger) {
	defer wg.Done()

	// We want to sync at least every `SyncInterval`. Being told to
	// sync, or completing a job, may intervene (in which case,
	// reschedule the next sync).
	syncTimer := time.NewTimer(d.SyncInterval)
	// Similarly checking to see if any controllers have new images
	// available.
	imagePollTimer := time.NewTimer(d.RegistryPollInterval)

	// Keep track of current, verified (if signature verification is
	// enabled), HEAD, so we can know when to treat a repo
	// mirror notification as a change. Otherwise, we'll just sync
	// every timer tick as well as every mirror refresh.
	syncHead := ""

	// Ask for a sync, and to poll images, straight away
	d.AskForSync()
	d.AskForImagePoll()

	for {
		var (
			lastKnownSyncTag = &lastKnownSyncTag{logger: logger, syncTag: d.GitConfig.SyncTag}
		)
		select {
		case <-stop:
			logger.Log("stopping", "true")
			return
		case <-d.pollImagesSoon:
			if !imagePollTimer.Stop() {
				select {
				case <-imagePollTimer.C:
				default:
				}
			}
			d.pollForNewImages(logger)
			imagePollTimer.Reset(d.RegistryPollInterval)
		case <-imagePollTimer.C:
			d.AskForImagePoll()
		case <-d.syncSoon:
			if !syncTimer.Stop() {
				select {
				case <-syncTimer.C:
				default:
				}
			}
			sync, err := d.NewSync(logger, syncHead)
			if err != nil {
				logger.Log("err", err)
				continue
			}
			err = sync.Run(context.Background(), lastKnownSyncTag)
			syncDuration.With(
				fluxmetrics.LabelSuccess, fmt.Sprint(err == nil),
			).Observe(time.Since(sync.started).Seconds())
			if err != nil {
				logger.Log("err", err)
			}
			syncTimer.Reset(d.SyncInterval)
		case <-syncTimer.C:
			d.AskForSync()
		case <-d.Repo.C:
			ctx, cancel := context.WithTimeout(context.Background(), d.GitConfig.Timeout)
			newSyncHead, invalidCommit, err := latestValidRevision(ctx, d.Repo, d.GitConfig)
			cancel()
			if err != nil {
				logger.Log("url", d.Repo.Origin().URL, "err", err)
				continue
			}
			if invalidCommit.Revision != "" {
				logger.Log("err", "found invalid GPG signature for commit", "revision", invalidCommit.Revision, "key", invalidCommit.Signature.Key)
			}
			logger.Log("event", "refreshed", "url", d.Repo.Origin().URL, "branch", d.GitConfig.Branch, "HEAD", newSyncHead)
			if newSyncHead != syncHead {
				syncHead = newSyncHead
				d.AskForSync()
			}
		case job := <-d.Jobs.Ready():
			queueLength.Set(float64(d.Jobs.Len()))
			jobLogger := log.With(logger, "jobID", job.ID)
			jobLogger.Log("state", "in-progress")
			// It's assumed that (successful) jobs will push commits
			// to the upstream repo, and therefore we probably want to
			// pull from there and sync the cluster afterwards.
			start := time.Now()
			err := job.Do(jobLogger)
			jobDuration.With(
				fluxmetrics.LabelSuccess, fmt.Sprint(err == nil),
			).Observe(time.Since(start).Seconds())
			if err != nil {
				jobLogger.Log("state", "done", "success", "false", "err", err)
			} else {
				jobLogger.Log("state", "done", "success", "true")
				ctx, cancel := context.WithTimeout(context.Background(), d.GitConfig.Timeout)
				err := d.Repo.Refresh(ctx)
				if err != nil {
					logger.Log("err", err)
				}
				cancel()
			}
		}
	}
}

// Ask for a sync, or if there's one waiting, let that happen.
func (d *LoopVars) AskForSync() {
	d.ensureInit()
	select {
	case d.syncSoon <- struct{}{}:
	default:
	}
}

// Ask for an image poll, or if there's one waiting, let that happen.
func (d *LoopVars) AskForImagePoll() {
	d.ensureInit()
	select {
	case d.pollImagesSoon <- struct{}{}:
	default:
	}
}

// -- internals to keep track of sync tag state
type lastKnownSyncTag struct {
	logger            log.Logger
	syncTag           string
	revision          string
	warnedAboutChange bool
}

func (s *lastKnownSyncTag) Revision() string {
	return s.revision
}

func (s *lastKnownSyncTag) SetRevision(oldRev, newRev string) {
	// Check if something other than the current instance of fluxd
	// changed the sync tag. This is likely caused by another instace
	// using the same tag. Having multiple instances fight for the same
	// tag can lead to fluxd missing manifest changes.
	if s.revision != "" && oldRev != s.revision && !s.warnedAboutChange {
		s.logger.Log("warning",
			"detected external change in git sync tag; the sync tag should not be shared by fluxd instances")
		s.warnedAboutChange = true
	}

	s.logger.Log("tag", s.syncTag, "old", oldRev, "new", newRev)
	s.revision = newRev
}
