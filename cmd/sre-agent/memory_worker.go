package main

import (
	"context"
	"fmt"
	"time"

	"github.com/y2/go-sre-agent/internal/llm"
	"github.com/y2/go-sre-agent/internal/memory"
)

// memoryWorker owns one model-processing goroutine. UI code receives status events only.
type memoryWorker struct {
	notify chan struct{}
	events chan string
	cancel context.CancelFunc
	done   chan struct{}
}

func startMemoryWorker(store *memory.Store, runDir string, client llm.ChatClient, opts memory.ProcessOptions) *memoryWorker {
	ctx, cancel := context.WithCancel(context.Background())
	w := &memoryWorker{notify: make(chan struct{}, 1), events: make(chan string, 8), cancel: cancel, done: make(chan struct{})}
	opts.RunDir = runDir
	if opts.Limit <= 0 {
		opts.Limit = 2
	}
	if opts.Timeout <= 0 {
		opts.Timeout = 30 * time.Second
	}
	go func() {
		defer close(w.done)
		defer close(w.events)
		delay := time.Duration(0)
		for {
			if delay > 0 {
				timer := time.NewTimer(delay)
				select {
				case <-ctx.Done():
					timer.Stop()
					return
				case <-w.notify:
					timer.Stop()
				case <-timer.C:
				}
			} else {
				select {
				case <-ctx.Done():
					return
				default:
				}
			}
			stats, err := store.Process(ctx, client, opts)
			if ctx.Err() != nil {
				return
			}
			if stats.Success > 0 || stats.Failed > 0 || stats.Stale > 0 {
				select {
				case w.events <- fmt.Sprintf("记忆处理：成功 %d，跳过 %d，过期 %d，失败 %d", stats.Success, stats.Skipped, stats.Stale, stats.Failed):
				default:
				}
			}
			if err != nil || stats.Failed > 0 {
				if delay == 0 {
					delay = 2 * time.Second
				} else {
					delay *= 2
					if delay > time.Minute {
						delay = time.Minute
					}
				}
				continue
			}
			if stats.Success >= opts.Limit {
				delay = 0
				continue
			}
			delay = 0
			select {
			case <-ctx.Done():
				return
			case <-w.notify:
			}
		}
	}()
	return w
}
func (w *memoryWorker) Notify() {
	if w != nil {
		select {
		case w.notify <- struct{}{}:
		default:
		}
	}
}
func (w *memoryWorker) Stop() {
	if w != nil && w.cancel != nil {
		w.cancel()
		<-w.done
	}
}

func unavailableMemoryWorker(err error) *memoryWorker {
	w := &memoryWorker{events: make(chan string, 1)}
	w.events <- fmt.Sprintf("模型记忆后台处理未启动：%v", err)
	return w
}

func (c *interactiveCLI) beginMemoryWorker() error {
	cfg, err := loadAppConfig(c.options.ConfigPath)
	if err != nil || !cfg.Memory.ModelEnabled {
		return err
	}
	provider, err := loadLLMConfig()
	if err != nil {
		c.memoryWorker = unavailableMemoryWorker(err)
		return nil
	}
	client, err := newChatClient(provider)
	if err != nil {
		c.memoryWorker = unavailableMemoryWorker(err)
		return nil
	}
	extractModel := cfg.Memory.ExtractModel
	if extractModel == "" {
		extractModel = provider.Model
	}
	consolidationModel := cfg.Memory.ConsolidationModel
	if consolidationModel == "" {
		consolidationModel = provider.Model
	}
	c.memoryWorker = startMemoryWorker(c.memoryStore.WithRunDir(c.config.RunDir), c.config.RunDir, client, memory.ProcessOptions{ExtractModel: extractModel, ConsolidationModel: consolidationModel, Timeout: cfg.Memory.Timeout, Limit: cfg.Memory.RoundLimit})
	return nil
}
