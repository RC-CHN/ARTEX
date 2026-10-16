package server

import (
	"context"
	"log"
	"time"
)

func (s *Server) resourceMaintenance(ctx context.Context) {
	ticker := time.NewTicker(30 * time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		run, cancel := context.WithTimeout(ctx, 20*time.Second)
		count, err := s.resourceLibrary().Sweep(run, time.Now().Add(-24*time.Hour))
		cancel()
		if err != nil {
			log.Printf("[resources] deferred cleanup failed; will retry next interval")
		} else if count > 0 {
			log.Printf("[resources] removed %d unreferenced library entries", count)
		}
	}
}
