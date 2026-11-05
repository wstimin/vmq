package maintenance

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func validInstallForm() url.Values {
	return url.Values{"token": {strings.Repeat("a", 64)}, "host": {"127.0.0.1"}, "port": {"3306"}, "database": {"vmq"}, "user": {"vmqfox"}, "password": {"private-db-password"}, "site_url": {"https://pay.example.com"}, "admin_user": {"admin"}, "admin_password": {"MyPrivatePassword!9"}, "confirm_password": {"MyPrivatePassword!9"}}
}

func TestInstallerProtectsSubmissionAndClosesAfterSuccess(t *testing.T) {
	for _, scenario := range []string{"bad token", "cross site", "invalid port", "password mismatch", "newline", "success", "database error", "too large"} {
		t.Run(scenario, func(t *testing.T) {
			calls := 0
			s := &installer{dir: t.TempDir(), token: strings.Repeat("a", 64), done: make(chan struct{}), install: func(_ context.Context, in InstallInput, _, _ string) error {
				calls++
				if in.Password != "private-db-password" || in.SiteURL != "https://pay.example.com" {
					t.Fatal("form values lost")
				}
				if scenario == "database error" {
					return errors.New("数据库不可用")
				}
				return nil
			}}
			form := validInstallForm()
			switch scenario {
			case "bad token":
				form.Set("token", "wrong")
			case "invalid port":
				form.Set("port", "0")
			case "password mismatch":
				form.Set("confirm_password", "different")
			case "newline":
				form.Set("password", "pass\nVMQ_TOKEN_SECRET=evil")
			case "too large":
				form.Set("password", strings.Repeat("x", 20000))
			}
			r := httptest.NewRequest(http.MethodPost, "http://pay.example.com/install", strings.NewReader(form.Encode()))
			r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			if scenario == "cross site" {
				r.Header.Set("Origin", "https://evil.example.com")
			}
			w := httptest.NewRecorder()
			s.ServeHTTP(w, r)
			if scenario == "success" {
				if calls != 1 || !s.complete || !strings.Contains(w.Body.String(), "安装完成") || !strings.Contains(w.Body.String(), "http-equiv=\"refresh\"") {
					t.Fatalf("installation failed: %d %s", w.Code, w.Body.String())
				}
				w2 := httptest.NewRecorder()
				s.ServeHTTP(w2, httptest.NewRequest(http.MethodGet, "/install", nil))
				if strings.Contains(w2.Body.String(), "MyPrivatePassword") {
					t.Fatal("password echoed")
				}
				duplicate := httptest.NewRequest(http.MethodPost, "/install", strings.NewReader(validInstallForm().Encode()))
				duplicate.Header.Set("Content-Type", "application/x-www-form-urlencoded")
				w3 := httptest.NewRecorder()
				s.ServeHTTP(w3, duplicate)
				if w3.Code != 409 || calls != 1 {
					t.Fatal("reinstallation was not locked")
				}
			} else if scenario == "database error" {
				if calls != 1 || s.complete {
					t.Fatal("database failure reported as success")
				}
			} else if calls != 0 || s.complete {
				t.Fatal("invalid request reached database initialization")
			}
			if strings.Contains(w.Body.String(), "private-db-password") || strings.Contains(w.Body.String(), "MyPrivatePassword!9") {
				t.Fatal("credentials reflected in installer response")
			}
		})
	}
}

func TestInstallerTokenAndExistingConfiguration(t *testing.T) {
	t.Setenv("VMQ_TOKEN_SECRET", "")
	t.Setenv("TOKEN_SECRET", "")
	dir := t.TempDir()
	token, err := installToken(dir)
	if err != nil || len(token) != 64 {
		t.Fatal("token not generated", err)
	}
	again, err := installToken(dir)
	if err != nil || token != again {
		t.Fatal("token changed after restart")
	}
	s := &installer{dir: dir, token: token}
	w := httptest.NewRecorder()
	s.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/install", nil))
	if strings.Contains(w.Body.String(), token) {
		t.Fatal("untrusted visitor learned installer secret")
	}
	needed, err := needsInstall(dir)
	if err != nil || !needed {
		t.Fatal("empty installation not detected")
	}
	if err := writeNewPrivate(filepath.Join(dir, ".env"), []byte("existing-secret")); err != nil {
		t.Fatal(err)
	}
	if err := writeNewPrivate(filepath.Join(dir, ".env"), []byte("overwrite")); err == nil {
		t.Fatal("existing config overwritten")
	}
	needed, err = needsInstall(dir)
	if err != nil || needed {
		t.Fatal("existing deployment exposed installation")
	}
	data, _ := os.ReadFile(filepath.Join(dir, ".env"))
	if string(data) != "existing-secret" {
		t.Fatal("config changed")
	}
	if err := os.Remove(filepath.Join(dir, ".env")); err != nil {
		t.Fatal(err)
	}
	if err := writePrivate(filepath.Join(dir, ".installed"), []byte("done")); err != nil {
		t.Fatal(err)
	}
	needed, err = needsInstall(dir)
	if err != nil || needed {
		t.Fatal("installation marker ignored")
	}
}

func TestInstallValidationAndEnvironmentPreserveSpecialCharacters(t *testing.T) {
	in := InstallInput{Host: "::1", Port: 3306, Database: "vmq", User: "user", Password: `p$#=\"' secret`, SiteURL: "https://pay.example.com/", AdminUser: "admin", AdminPassword: "AdminPass99"}
	if err := in.validate(); err != nil {
		t.Fatal(err)
	}
	data := string(environment(in, "generated-key", "8080"))
	if !strings.Contains(data, "VMQ_DB_PASSWORD=\""+in.Password+"\"\n") || strings.Contains(data, in.AdminPassword) {
		t.Fatal("database password lost or plaintext administrator persisted")
	}
	for _, site := range []string{"https://user:pass@example.com", "javascript:alert(1)", "https://example.com/sub", "https://example.com/?key=secret"} {
		copy := in
		copy.SiteURL = site
		if copy.validate() == nil {
			t.Fatalf("unsafe site accepted: %s", site)
		}
	}
}
