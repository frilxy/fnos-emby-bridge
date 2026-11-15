package fn_test

import (
	"net/http/httptest"
	"testing"

	"fnos-emby-bridge/internal/mock"
)

// mockServer 包一层 httptest，便于测试里 defer 关闭。
type mockServer struct {
	*httptest.Server
}

func (m *mockServer) start(t *testing.T) {
	t.Helper()
	m.Server = httptest.NewServer(mock.NewHandler(false, ""))
	t.Cleanup(m.Close)
}
