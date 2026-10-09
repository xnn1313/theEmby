package gateway

import (
	"context"
	"fmt"
	"log"

	"nextemby-replay/store"
)

// logSink 是 engine.LogFunc 与网关自身事件的异步落库通道：
// 播放决策中文日志经 engine 触发，banned/过期/超限拒绝与 Stopped 信号由网关
// 直接打。ch 满时丢弃（不阻塞播放）；db 未接入时不启用。
type logMsg struct {
	category string
	message  string
}

type logSink struct {
	ch     chan logMsg
	st     *store.Store
	logger *log.Logger
}

func newLogSink(st *store.Store, logger *log.Logger) *logSink {
	s := &logSink{ch: make(chan logMsg, 1000), st: st, logger: logger}
	go s.run()
	return s
}

func (s *logSink) run() {
	ctx := context.Background()
	for m := range s.ch {
		if err := s.st.WriteLog(ctx, m.category, m.message); err != nil {
			s.logger.Printf("logsink 写入失败: %v", err)
		}
	}
}

// async 非阻塞入队；队列满（1000）时丢弃，保证播放链路不受影响。
func (s *logSink) async(category, message string) {
	select {
	case s.ch <- logMsg{category: category, message: message}:
	default:
	}
}

// AttachLogSink 接入日志 sink：建异步落库通道，并把 engine.LogFunc 接到它上面。
// st 为 nil（DB 未接入）时不启用，engine.LogFunc 保持原样（测试回退）。
func (s *Server) AttachLogSink(st *store.Store) {
	if st == nil {
		return
	}
	sink := newLogSink(st, s.logger)
	s.sink = sink
	s.engine.LogFunc = func(category, message string) {
		// engine 同步调用 hook；sink.async 永不阻塞。
		sink.async(category, message)
	}
}

// logUser 打一条 category=user 的网关事件日志（banned/过期/超限拒绝）。
// sink 未启用时静默。
func (s *Server) logUser(format string, args ...any) {
	if s.sink != nil {
		s.sink.async("user", fmt.Sprintf(format, args...))
	}
}

// logPlay 打一条 category=play 的网关事件日志（如 Stopped 信号）。
// sink 未启用时静默。
func (s *Server) logPlay(format string, args ...any) {
	if s.sink != nil {
		s.sink.async("play", fmt.Sprintf(format, args...))
	}
}
