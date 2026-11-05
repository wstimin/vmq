package http

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/hulisang/vmqfox-backend/internal/auth"
	"github.com/hulisang/vmqfox-backend/internal/maintenance"
)

type denyUpdateToken struct{}

func (denyUpdateToken) ParseAuthorization(string) (auth.Identity, error) {
	return auth.Identity{}, errors.New("invalid")
}

func TestUpdateRoutesRequireAdministratorAuthentication(t *testing.T) {
	gin.SetMode(gin.TestMode)
	u, err := maintenance.NewUpdater(t.TempDir(), "unused", nil)
	if err != nil {
		t.Fatal(err)
	}
	router := NewRouter(RouterDeps{Updater: u, TokenParser: denyUpdateToken{}})
	for _, path := range []string{"status", "source", "check", "start"} {
		method := http.MethodPost
		if path == "status" {
			method = http.MethodGet
		}
		w := httptest.NewRecorder()
		router.ServeHTTP(w, httptest.NewRequest(method, "/api/system/update/"+path, nil))
		if w.Code != 401 {
			t.Fatalf("unauthenticated %s returned %d", path, w.Code)
		}
	}
}
