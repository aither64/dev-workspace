package web

import (
	"context"
	"errors"
	"github.com/aither64/dev-workspace/portal/internal/session"
	"time"
)

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
