package web

import (
	"container/list"
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/aither64/dev-workspace/portal/internal/session"
)

const repositoryReviewCacheBytes = 64 * 1024 * 1024

type reviewCacheEntry struct {
	key   string
	value any
	bytes int
}

type reviewCacheCall struct {
	done    chan struct{}
	cancel  context.CancelFunc
	waiters int
	value   any
	err     error
}

// cachedReview stores immutable values only. Callers must not modify returned
// slices or pointers. The conservative charge includes serialized payloads plus
// room for Go object overhead; discarded temporary JSON is not retained.
func cachedReview[T any](ctx context.Context, service *repositoryReviewService, key string, read func(context.Context) (T, error)) (T, error) {
	var zero T
	if err := ctx.Err(); err != nil {
		return zero, err
	}
	service.cacheMu.Lock()
	if entry := service.cache[key]; entry != nil {
		service.cacheLRU.MoveToFront(entry)
		value := entry.Value.(reviewCacheEntry).value.(T)
		service.cacheMu.Unlock()
		return value, nil
	}
	call := service.calls[key]
	if call == nil {
		workCtx, cancel := context.WithTimeout(service.lifetime, 15*time.Second)
		call = &reviewCacheCall{done: make(chan struct{}), cancel: cancel}
		service.calls[key] = call
		go func() {
			value, err := read(workCtx)
			encoded, marshalErr := json.Marshal(value)
			charge := len(key) + len(encoded)*2 + 512
			service.cacheMu.Lock()
			call.value, call.err = value, err
			if err == nil && marshalErr == nil && workCtx.Err() == nil && charge <= repositoryReviewCacheBytes {
				for service.cacheBytes+charge > repositoryReviewCacheBytes && service.cacheLRU.Len() > 0 {
					oldest := service.cacheLRU.Back()
					entry := oldest.Value.(reviewCacheEntry)
					delete(service.cache, entry.key)
					service.cacheBytes -= entry.bytes
					service.cacheLRU.Remove(oldest)
				}
				service.cache[key] = service.cacheLRU.PushFront(reviewCacheEntry{key: key, value: value, bytes: charge})
				service.cacheBytes += charge
			}
			if service.calls[key] == call {
				delete(service.calls, key)
			}
			close(call.done)
			service.cacheMu.Unlock()
			cancel()
		}()
	}
	call.waiters++
	service.cacheMu.Unlock()
	defer func() {
		service.cacheMu.Lock()
		call.waiters--
		if call.waiters == 0 {
			call.cancel()
			if service.calls[key] == call {
				delete(service.calls, key)
			}
		}
		service.cacheMu.Unlock()
	}()
	select {
	case <-ctx.Done():
		return zero, ctx.Err()
	case <-call.done:
		if call.err != nil {
			return zero, call.err
		}
		return call.value.(T), nil
	}
}

func (s *Server) discoverRepositories(ctx context.Context, force bool) (map[string][]session.Repository, error) {
	service := s.reviews()
	for {
		service.discoveryMu.Lock()
		if !force && !service.discoveryAt.IsZero() && time.Since(service.discoveryAt) < 5*time.Second {
			value, err := service.discovery, service.discoveryErr
			service.discoveryMu.Unlock()
			return value, err
		}
		if wait := service.discoveryWait; wait != nil {
			service.discoveryMu.Unlock()
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-wait:
			}
			force = false
			continue
		}
		wait := make(chan struct{})
		service.discoveryWait = wait
		service.discoveryMu.Unlock()
		value, err := session.DiscoverActiveRepositoriesWithLimitContext(ctx, s.config.Workspace, service.reader.Jobs)
		service.discoveryMu.Lock()
		if ctx.Err() == nil {
			service.discovery, service.discoveryErr, service.discoveryAt = value, err, time.Now()
		}
		service.discoveryWait = nil
		close(wait)
		service.discoveryMu.Unlock()
		return value, err
	}
}

func (s *Server) activeRepositories(ctx context.Context, summary *session.Summary) ([]session.Repository, error) {
	discovered, err := s.discoverRepositories(ctx, false)
	result, mergeErr := session.MergeActiveRepositories(summary.Repositories, discovered[summary.Slug])
	return result, errors.Join(err, mergeErr)
}

// The map and list have the same lifetime as the portal; they contain no saved
// credentials, mutable refs, or authorization decisions.
func (service *repositoryReviewService) initializeCache() {
	if service.lifetime == nil {
		service.lifetime = context.Background()
	}
	service.cache = make(map[string]*list.Element)
	service.cacheLRU = list.New()
	service.calls = make(map[string]*reviewCacheCall)
}
