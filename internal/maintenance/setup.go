package maintenance

import (
	"context"
	"crypto/subtle"
	_ "embed"
	"errors"
	"fmt"
	"html/template"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

//go:embed setup.html
var setupHTML string
var setupPage = template.Must(template.New("setup").Parse(setupHTML))

type InstallInput struct {
	Host, Database, User, Password    string
	Port                              int
	SiteURL, AdminUser, AdminPassword string
}

func (in *InstallInput) validate() error {
	in.Host = strings.TrimSpace(in.Host)
	in.Database = strings.TrimSpace(in.Database)
	in.User = strings.TrimSpace(in.User)
	in.AdminUser = strings.TrimSpace(in.AdminUser)
	in.SiteURL = strings.TrimRight(strings.TrimSpace(in.SiteURL), "/")
	for _, value := range []string{in.Host, in.Database, in.User, in.Password, in.AdminUser, in.AdminPassword, in.SiteURL} {
		if strings.ContainsAny(value, "\r\n\x00") || len(value) > 2048 {
			return errors.New("输入中不能包含换行或控制字符")
		}
	}
	if in.Host == "" || strings.ContainsAny(in.Host, "/\\@?#") || in.Port < 1 || in.Port > 65535 {
		return errors.New("请填写正确的数据库地址和端口")
	}
	if in.Database == "" || len(in.Database) > 64 || strings.ContainsAny(in.Database, "`/\\.") || in.User == "" {
		return errors.New("请填写正确的数据库名称和用户名")
	}
	if in.AdminUser == "" || len(in.AdminUser) > 128 || len(in.AdminPassword) < 8 || len(in.AdminPassword) > 72 {
		return errors.New("管理员用户名不能为空，密码长度须为 8～72 字节")
	}
	u, err := url.Parse(in.SiteURL)
	if err != nil || u.Host == "" || u.User != nil || (u.Scheme != "http" && u.Scheme != "https") || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
		return errors.New("网站地址请填写 http://IP:端口 或 https://域名，不要添加路径")
	}
	return nil
}

func needsInstall(dir string) (bool, error) {
	for _, path := range []string{filepath.Join(dir, ".env"), filepath.Join(dir, "..", ".env"), filepath.Join(dir, ".installed")} {
		if _, err := os.Stat(path); err == nil {
			return false, nil
		} else if !errors.Is(err, os.ErrNotExist) {
			return false, err
		}
	}
	return os.Getenv("VMQ_TOKEN_SECRET") == "" && os.Getenv("TOKEN_SECRET") == "", nil
}

func installToken(dir string) (string, error) {
	path := filepath.Join(dir, ".install-token")
	data, err := os.ReadFile(path)
	if err == nil {
		token := strings.TrimSpace(string(data))
		decoded, decodeErr := hexSecret(token)
		if decodeErr != nil || len(decoded) != 32 {
			return "", errors.New("安装口令文件无效，请检查 .install-token")
		}
		return token, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	token, err := randomSecret()
	if err != nil {
		return "", err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if errors.Is(err, os.ErrExist) {
		return installToken(dir)
	}
	if err != nil {
		return "", err
	}
	_, err = f.WriteString(token)
	closeErr := f.Close()
	if err != nil {
		return "", err
	}
	return token, closeErr
}

type installer struct {
	dir, token string
	done       chan struct{}
	mu         sync.Mutex
	complete   bool
	install    func(context.Context, InstallInput, string, string) error
}

func (s *installer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("X-Frame-Options", "DENY")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; form-action 'self'; frame-ancestors 'none'; base-uri 'none'")
	if r.URL.Path != "/" && r.URL.Path != "/install" {
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	data := struct {
		Token, Error, Host, SiteURL, Repository string
		Done                                    bool
	}{Host: "127.0.0.1", Repository: DefaultRepository}
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	data.SiteURL = scheme + "://" + r.Host
	token := r.URL.Query().Get("token")
	if r.Method == http.MethodPost {
		r.Body = http.MaxBytesReader(w, r.Body, 16<<10)
		if err := r.ParseForm(); err != nil {
			http.Error(w, "表单过大或格式错误", 400)
			return
		}
		token = r.PostForm.Get("token")
		if origin := r.Header.Get("Origin"); origin != "" {
			u, err := url.Parse(origin)
			if err != nil || !strings.EqualFold(u.Host, r.Host) {
				http.Error(w, "不允许跨站提交安装信息", 403)
				return
			}
		}
	}
	valid := len(token) == 64 && subtle.ConstantTimeCompare([]byte(token), []byte(s.token)) == 1
	if valid {
		data.Token = token
	}
	if r.Method == http.MethodPost {
		if !valid {
			w.WriteHeader(http.StatusForbidden)
			data.Error = "安装口令不正确，请使用启动时显示的专属安装链接"
		} else {
			s.mu.Lock()
			defer s.mu.Unlock()
			if s.complete {
				http.Error(w, "安装已完成，入口已锁定", 409)
				return
			}
			port, _ := strconv.Atoi(r.PostForm.Get("port"))
			in := InstallInput{Host: r.PostForm.Get("host"), Port: port, Database: r.PostForm.Get("database"), User: r.PostForm.Get("user"), Password: r.PostForm.Get("password"), SiteURL: r.PostForm.Get("site_url"), AdminUser: r.PostForm.Get("admin_user"), AdminPassword: r.PostForm.Get("admin_password")}
			if err := in.validate(); err != nil {
				data.Error = err.Error()
			} else if in.AdminPassword != r.PostForm.Get("confirm_password") {
				data.Error = "两次管理员密码不一致"
			} else {
				ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
				err := s.install(ctx, in, s.dir, s.token)
				cancel()
				if err != nil {
					data.Error = err.Error()
				} else {
					s.complete, data.Done = true, true
					close(s.done)
				}
			}
		}
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_ = setupPage.Execute(w, data)
}

// RunSetup returns to normal startup only after configuration and administrator are committed.
func RunSetup(ctx context.Context, logger *log.Logger) error {
	dir, err := os.Getwd()
	if err != nil {
		return err
	}
	needed, err := needsInstall(dir)
	if err != nil || !needed {
		return err
	}
	token, err := installToken(dir)
	if err != nil {
		return err
	}
	port := os.Getenv("VMQ_SERVER_PORT")
	if port == "" {
		port = "8080"
	}
	portNumber, err := strconv.Atoi(port)
	if err != nil || portNumber < 1 || portNumber > 65535 {
		return errors.New("VMQ_SERVER_PORT 无效")
	}
	host := os.Getenv("VMQ_SERVER_HOST")
	if host == "" {
		host = "0.0.0.0"
	}
	listener, err := net.Listen("tcp", net.JoinHostPort(host, port))
	if err != nil {
		return err
	}
	s := &installer{dir: dir, token: token, done: make(chan struct{}), install: initializeDatabase}
	server := &http.Server{Handler: s, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 15 * time.Second, WriteTimeout: 70 * time.Second, IdleTimeout: 30 * time.Second}
	logger.Printf("首次安装：浏览器打开 http://服务器IP:%s/install?token=%s （请保管此专属链接）", port, token)
	failures := make(chan error, 1)
	go func() { failures <- server.Serve(listener) }()
	select {
	case <-ctx.Done():
		_ = server.Close()
		return ctx.Err()
	case err := <-failures:
		return fmt.Errorf("安装服务停止: %w", err)
	case <-s.done:
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := server.Shutdown(shutdown); err != nil {
			return err
		}
		_ = os.Remove(filepath.Join(dir, ".install-token"))
		return nil
	}
}
