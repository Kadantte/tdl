package dl

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/AlecAivazis/survey/v2"
	"github.com/fatih/color"
	"github.com/gotd/contrib/middleware/floodwait"
	"github.com/iyear/tdl/app/internal/dliter"
	"github.com/iyear/tdl/app/internal/tgc"
	"github.com/iyear/tdl/pkg/consts"
	"github.com/iyear/tdl/pkg/dcpool"
	"github.com/iyear/tdl/pkg/downloader"
	"github.com/iyear/tdl/pkg/key"
	"github.com/iyear/tdl/pkg/kv"
	"github.com/iyear/tdl/pkg/logger"
	"github.com/jedib0t/go-pretty/v6/text"
	"github.com/spf13/viper"
	"go.uber.org/multierr"
	"go.uber.org/zap"
	"time"
)

type Options struct {
	Dir        string
	RewriteExt bool
	SkipSame   bool
	Template   string
	URLs       []string
	Files      []string
	Include    []string
	Exclude    []string
	Desc       bool
	PoolSize   int64
}

type parser struct {
	Data   []string
	Parser func(ctx context.Context, pool dcpool.Pool, kvd kv.KV, data []string) ([]*dliter.Dialog, error)
}

func Run(ctx context.Context, opts *Options) error {
	c, kvd, err := tgc.NoLogin(ctx)
	if err != nil {
		return err
	}

	return tgc.RunWithAuth(ctx, c, func(ctx context.Context) (rerr error) {
		color.Green("Preparing DC pool... It may take a while. size: %d", opts.PoolSize)

		start := time.Now()
		pool, err := dcpool.NewPool(ctx, c, opts.PoolSize, floodwait.NewSimpleWaiter())
		if err != nil {
			return err
		}
		defer multierr.AppendInvoke(&rerr, multierr.Close(pool))

		// clear prepare message
		fmt.Printf("%s%s", text.CursorUp.Sprint(), text.EraseLine.Sprint())
		color.Green("DC pool prepared in %s", time.Since(start))

		parsers := []parser{
			{Data: opts.URLs, Parser: parseURLs},
			{Data: opts.Files, Parser: parseFiles},
		}
		dialogs, err := collectDialogs(ctx, pool, kvd, parsers)
		if err != nil {
			return err
		}
		logger.From(ctx).Debug("Collect dialogs",
			zap.Any("dialogs", dialogs))

		iter, err := dliter.New(&dliter.Options{
			Pool:     pool,
			KV:       kvd,
			Template: opts.Template,
			Include:  opts.Include,
			Exclude:  opts.Exclude,
			Desc:     opts.Desc,
			Dialogs:  dialogs,
		})
		if err != nil {
			return err
		}

		// resume download and ask user to continue
		if err = resume(ctx, kvd, iter); err != nil {
			return err
		}
		defer func() { // save progress
			if rerr != nil { // download is interrupted
				multierr.AppendInto(&rerr, saveProgress(ctx, kvd, iter))
			} else { // if finished, we should clear resume key
				multierr.AppendInto(&rerr, kvd.Delete(key.Resume(iter.Fingerprint())))
			}
		}()

		options := &downloader.Options{
			Pool:       pool,
			Dir:        opts.Dir,
			RewriteExt: opts.RewriteExt,
			SkipSame:   opts.SkipSame,
			PartSize:   viper.GetInt(consts.FlagPartSize),
			Threads:    viper.GetInt(consts.FlagThreads),
			Iter:       iter,
		}
		limit := viper.GetInt(consts.FlagLimit)

		logger.From(ctx).Info("Start download",
			zap.String("dir", options.Dir),
			zap.Bool("rewrite_ext", options.RewriteExt),
			zap.Bool("skip_same", options.SkipSame),
			zap.Int("part_size", options.PartSize),
			zap.Int("threads", options.Threads),
			zap.Int("limit", limit))

		return downloader.New(options).Download(ctx, limit)
	})
}

func collectDialogs(ctx context.Context, pool dcpool.Pool, kvd kv.KV, parsers []parser) ([][]*dliter.Dialog, error) {
	var dialogs [][]*dliter.Dialog
	for _, p := range parsers {
		d, err := p.Parser(ctx, pool, kvd, p.Data)
		if err != nil {
			return nil, err
		}
		dialogs = append(dialogs, d)
	}
	return dialogs, nil
}

func resume(ctx context.Context, kvd kv.KV, iter *dliter.Iter) error {
	logger.From(ctx).Debug("Check resume key",
		zap.String("fingerprint", iter.Fingerprint()))

	b, err := kvd.Get(key.Resume(iter.Fingerprint()))
	if err != nil && !errors.Is(err, kv.ErrNotFound) {
		return err
	}
	if len(b) == 0 { // no progress
		return nil
	}

	finished := make(map[int]struct{})
	if err = json.Unmarshal(b, &finished); err != nil {
		return err
	}

	// finished is empty, no need to resume
	if len(finished) == 0 {
		return nil
	}

	confirm := false
	if err = survey.AskOne(&survey.Confirm{
		Message: fmt.Sprintf("Found unfinished download, continue from '%d/%d'?", len(finished), iter.Total(ctx)),
	}, &confirm); err != nil {
		return err
	}

	logger.From(ctx).Debug("Resume download",
		zap.Int("finished", len(finished)))

	if !confirm {
		// clear resume key
		return kvd.Delete(key.Resume(iter.Fingerprint()))
	}

	iter.SetFinished(finished)
	return nil
}

func saveProgress(ctx context.Context, kvd kv.KV, it *dliter.Iter) error {
	finished := it.Finished()
	logger.From(ctx).Debug("Save progress",
		zap.Int("finished", len(finished)))

	b, err := json.Marshal(finished)
	if err != nil {
		return err
	}
	return kvd.Set(key.Resume(it.Fingerprint()), b)
}
