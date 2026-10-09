package store

import (
	"context"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"nextemby-replay/engine"
)

// TestConcurrentWrites：决策落库与日志 sink 并发写不应出现 SQLITE_BUSY
// （busy_timeout 必须对连接池里每个新连接生效，见 Open 注释）。
func TestConcurrentWrites(t *testing.T) {
	ctx := context.Background()
	st, err := Open(filepath.Join(t.TempDir(), "c.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })

	var wg sync.WaitGroup
	errCh := make(chan error, 200)
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for j := 0; j < 5; j++ {
				if err := st.LogDecision(engine.DecisionSummary{
					UserID: "u", Branch: engine.BranchCacheHit, Allowed: true,
				}); err != nil {
					errCh <- err
					return
				}
				if err := st.WriteLog(ctx, "play", "msg"); err != nil {
					errCh <- err
					return
				}
			}
		}(i)
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		if strings.Contains(err.Error(), "SQLITE_BUSY") || strings.Contains(err.Error(), "database is locked") {
			t.Fatalf("concurrent write busy: %v", err)
		}
		t.Fatalf("concurrent write: %v", err)
	}
}
