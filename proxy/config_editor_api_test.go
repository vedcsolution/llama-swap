package proxy

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

func TestConfigEditorAPI_RejectsInvalidBashPayload(t *testing.T) {
	gin.SetMode(gin.TestMode)

	root := t.TempDir()
	cfgPath := filepath.Join(root, "config.yaml")
	initial := "" +
		"models: {}\n" +
		"groups: {}\n" +
		"macros:\n" +
		"  user_home: /home/tester\n"
	if err := os.WriteFile(cfgPath, []byte(initial), 0o644); err != nil {
		t.Fatalf("write initial config: %v", err)
	}

	pm := &ProxyManager{configPath: cfgPath}
	router := gin.New()
	router.POST("/api/config/editor", pm.apiSaveConfigEditor)

	invalidContent := "" +
		"models:\n" +
		"  bad-model:\n" +
		"    cmd: \"bash -lc 'if true; then echo ok; fi echo bad'\"\n" +
		"    cmdStop: \"bash -lc 'true'\"\n" +
		"    proxy: \"http://127.0.0.1:9001\"\n" +
		"groups: {}\n"

	body := []byte(`{"content":` + quoteJSONString(invalidContent) + `}`)
	req := httptest.NewRequest(http.MethodPost, "/api/config/editor", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected status 400, got %d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "invalid bash -lc payload") {
		t.Fatalf("expected bash payload validation error, got body=%s", rec.Body.String())
	}
}

func quoteJSONString(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '\\':
			b.WriteString(`\\`)
		case '"':
			b.WriteString(`\"`)
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		case '\t':
			b.WriteString(`\t`)
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
	return b.String()
}
