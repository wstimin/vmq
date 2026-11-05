package maintenance

import (
	"context"
	"crypto/sha256"
	"debug/elf"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
)

type updateConfig struct {
	Repository string `json:"repository"`
	Token      string `json:"token,omitempty"`
}
type UpdateState struct {
	Phase           string `json:"phase"`
	Progress        int    `json:"progress"`
	Message         string `json:"message"`
	CurrentVersion  string `json:"current_version"`
	LatestVersion   string `json:"latest_version"`
	PreviousVersion string `json:"previous_version,omitempty"`
	Repository      string `json:"repository"`
	ReleaseURL      string `json:"release_url,omitempty"`
	Available       bool   `json:"available"`
	HasToken        bool   `json:"has_token"`
	Managed         bool   `json:"managed"`
	Attempts        int    `json:"attempts,omitempty"`
}

type Updater struct {
	mu              sync.Mutex
	op              sync.Mutex
	dir, executable string
	config          updateConfig
	state           UpdateState
	client          *http.Client
	restart         func() error
	managed         bool
}

func NewUpdater(dir, executable string, restart func() error) (*Updater, error) {
	u := &Updater{dir: dir, executable: executable, client: githubClient(), restart: restart, managed: runtime.GOOS == "linux" && os.Getenv("VMQ_MANAGED_RESTART") == "1"}
	u.config.Repository = DefaultRepository
	if data, err := os.ReadFile(filepath.Join(dir, ".update-source.json")); err == nil {
		if json.Unmarshal(data, &u.config) != nil {
			return nil, errors.New("更新源配置文件无效")
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	repository, err := normalizeRepository(u.config.Repository)
	if err != nil {
		return nil, err
	}
	u.config.Repository = repository
	u.state = UpdateState{Phase: "idle", Message: "尚未检查更新"}
	if data, err := os.ReadFile(filepath.Join(dir, ".update-state.json")); err == nil {
		_ = json.Unmarshal(data, &u.state)
	}
	if isBusy(u.state.Phase) && u.state.Phase != "restarting" {
		u.state.Phase = "failed"
		u.state.Message = "上次更新在重启前中断，原程序保留，可重新检查更新"
	}
	return u, nil
}

func isBusy(phase string) bool {
	return phase == "downloading" || phase == "verifying" || phase == "installing" || phase == "restarting"
}

func (u *Updater) Info() UpdateState {
	u.mu.Lock()
	defer u.mu.Unlock()
	s := u.state
	s.CurrentVersion, s.Repository, s.HasToken, s.Managed = Version, u.config.Repository, u.config.Token != "", u.managed
	return s
}

func (u *Updater) setState(phase string, progress int, message string) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.state.Phase, u.state.Progress, u.state.Message = phase, progress, message
	data, _ := json.Marshal(u.state)
	// Progress is polled from memory; persist only transitions to avoid writes per network chunk.
	if phase != "downloading" || progress == 0 {
		_ = writePrivate(filepath.Join(u.dir, ".update-state.json"), data)
	}
}

func (u *Updater) SaveSource(repository, token string) error {
	if !u.op.TryLock() {
		return errors.New("更新操作正在进行，请稍后重试")
	}
	defer u.op.Unlock()
	if isBusy(u.Info().Phase) {
		return errors.New("正在更新，请完成后再修改更新源")
	}
	repository, err := normalizeRepository(repository)
	if err != nil {
		return err
	}
	if len(token) > 512 || strings.ContainsAny(token, "\r\n\x00") {
		return errors.New("GitHub 令牌格式无效")
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	cfg := u.config
	cfg.Repository = repository
	if token != "" {
		cfg.Token = token
	}
	data, _ := json.Marshal(cfg)
	if err := writePrivate(filepath.Join(u.dir, ".update-source.json"), data); err != nil {
		return errors.New("更新源保存失败，请检查目录写权限")
	}
	u.config = cfg
	u.state = UpdateState{Phase: "idle", Message: "更新源已保存，请检测新版本"}
	return nil
}

func (u *Updater) Check(ctx context.Context) (UpdateState, error) {
	if !u.op.TryLock() {
		return u.Info(), errors.New("更新操作正在进行，请稍后重试")
	}
	defer u.op.Unlock()
	if isBusy(u.Info().Phase) {
		return u.Info(), errors.New("正在更新")
	}
	u.setState("checking", 0, "正在连接 GitHub 检测版本")
	_, tag, _, err := u.latest(ctx)
	if err != nil {
		u.setState("failed", 0, err.Error())
		return u.Info(), err
	}
	u.mu.Lock()
	u.state.LatestVersion, u.state.Available, u.state.ReleaseURL = tag, newer(tag, Version), releaseURL(u.config.Repository, tag)
	u.mu.Unlock()
	if newer(tag, Version) {
		u.setState("available", 0, "发现新版本，点击更新开始安装")
	} else {
		u.setState("current", 100, "当前已是最新版本")
	}
	return u.Info(), nil
}

func (u *Updater) Start(ctx context.Context) error {
	if !u.op.TryLock() {
		return errors.New("更新操作正在进行，请稍后重试")
	}
	defer u.op.Unlock()
	if !u.managed || u.restart == nil {
		return errors.New("在线更新需要由安装包的 start.sh 配置开机服务后启动")
	}
	if isBusy(u.Info().Phase) {
		return errors.New("更新任务已在运行")
	}
	asset, tag, digest, err := u.latest(ctx)
	if err != nil {
		return err
	}
	if !newer(tag, Version) {
		return errors.New("没有可安装的新版本")
	}
	u.mu.Lock()
	u.state.LatestVersion = tag
	u.state.PreviousVersion = Version
	u.state.Attempts = 0
	u.state.Available = true
	u.mu.Unlock()
	u.setState("downloading", 0, "正在下载新版本")
	go u.run(asset, tag, digest)
	return nil
}

func (u *Updater) run(asset releaseAsset, tag, digest string) {
	u.op.Lock()
	defer u.op.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	if err := u.downloadAndReplace(ctx, asset, tag, digest); err != nil {
		u.setState("failed", 0, err.Error())
		return
	}
	u.setState("restarting", 98, "新版本已就绪，正在重启；连接恢复后页面会自动刷新")
	if err := u.persist(); err != nil {
		_ = restoreBinary(u.executable)
		u.setState("failed", 0, "无法保存更新状态，已恢复原程序")
		return
	}
	// Give the browser time to read the persisted restart state before draining HTTP requests.
	time.Sleep(time.Second)
	if err := u.restart(); err != nil {
		if rollbackErr := restoreBinary(u.executable); rollbackErr != nil {
			u.setState("failed", 0, "重启和恢复失败，请检查服务日志")
			return
		}
		u.setState("failed", 0, "无法触发服务重启，已恢复原程序")
	}
}

type progressWriter struct {
	u              *Updater
	written, total int64
	percent        int
}

func (p *progressWriter) Write(data []byte) (int, error) {
	p.written += int64(len(data))
	n := int(p.written * 85 / p.total)
	if n > 85 {
		n = 85
	}
	if n > p.percent {
		p.percent = n
		p.u.setState("downloading", n, fmt.Sprintf("正在下载：%.1f / %.1f MB", float64(p.written)/(1<<20), float64(p.total)/(1<<20)))
	}
	return len(data), nil
}

func (u *Updater) downloadAndReplace(ctx context.Context, asset releaseAsset, tag, digest string) error {
	response, err := u.github(ctx, "/repos/"+u.config.Repository+"/releases/assets/"+strconv.FormatInt(asset.ID, 10), "application/octet-stream")
	if err != nil {
		return err
	}
	defer response.Body.Close()
	file, err := os.CreateTemp(filepath.Dir(u.executable), ".vmq-update-*")
	if err != nil {
		return errors.New("程序目录不可写，无法保存更新")
	}
	defer os.Remove(file.Name())
	hasher := sha256.New()
	_, copyErr := io.Copy(io.MultiWriter(file, hasher, &progressWriter{u: u, total: asset.Size}), io.LimitReader(response.Body, asset.Size+1))
	if copyErr == nil {
		copyErr = file.Sync()
	}
	closeErr := file.Close()
	if copyErr != nil || closeErr != nil {
		return errors.New("下载中断或写入失败，原程序未修改")
	}
	u.setState("verifying", 88, "正在校验文件完整性和 CPU 架构")
	info, err := os.Stat(file.Name())
	if err != nil || info.Size() != asset.Size || hex.EncodeToString(hasher.Sum(nil)) != digest {
		return errors.New("更新文件大小或 SHA-256 校验失败，原程序未修改")
	}
	if err := validateBinary(file.Name()); err != nil {
		return err
	}
	if err := fileMode(file.Name()); err != nil {
		return err
	}
	u.setState("installing", 94, "正在保存原程序并切换到新版本")
	if err := u.persist(); err != nil {
		return errors.New("更新状态无法保存，原程序未修改")
	}
	backup := u.executable + ".previous"
	if err := os.Remove(backup); err != nil && !errors.Is(err, os.ErrNotExist) {
		return errors.New("无法清理上次程序备份")
	}
	if err := os.Link(u.executable, backup); err != nil {
		return errors.New("备份原程序失败，更新已停止")
	}
	if err := os.Rename(file.Name(), u.executable); err != nil {
		return errors.New("切换失败，原程序未修改")
	}
	return nil
}

func fileMode(path string) error {
	if os.Chmod(path, 0755) != nil {
		return errors.New("无法设置新程序执行权限")
	}
	return nil
}

func validateBinary(path string) error {
	f, err := elf.Open(path)
	if err != nil {
		return errors.New("更新文件不是有效的 Linux 程序")
	}
	defer f.Close()
	want := elf.EM_X86_64
	if runtime.GOARCH == "arm64" {
		want = elf.EM_AARCH64
	}
	if (runtime.GOARCH != "amd64" && runtime.GOARCH != "arm64") || f.Class != elf.ELFCLASS64 || f.Data != elf.ELFDATA2LSB || f.Machine != want {
		return errors.New("更新程序 CPU 架构不匹配")
	}
	return nil
}

func restoreBinary(executable string) error { return os.Rename(executable+".previous", executable) }

// RecoverUpdate is called before configuration/database startup. The service supervisor restarts
// after a crash; one unsuccessful attempt triggers restoration of the previous executable.
func RecoverUpdate(dir, executable string) (bool, error) {
	path := filepath.Join(dir, ".update-state.json")
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	var state UpdateState
	if json.Unmarshal(data, &state) != nil {
		return false, errors.New("更新状态文件损坏，请检查 .update-state.json")
	}
	if state.Phase == "installing" && Version == state.PreviousVersion {
		state.Phase, state.Message = "failed", "更新在程序切换前中断，原程序保持可用"
		out, _ := json.Marshal(state)
		return false, writePrivate(path, out)
	}
	if state.Phase == "installing" {
		state.Phase = "restarting"
	}
	if state.Phase != "restarting" {
		return false, nil
	}
	if state.Attempts == 0 {
		state.Attempts = 1
	} else {
		if err := restoreBinary(executable); err != nil {
			return false, fmt.Errorf("更新启动失败且无法恢复备份: %w", err)
		}
		state.Phase, state.Progress, state.Message = "rolled_back", 0, "新版本未能启动，已自动恢复原程序"
		state.Available = false
	}
	out, _ := json.Marshal(state)
	if err := writePrivate(path, out); err != nil {
		return false, err
	}
	return state.Phase == "rolled_back", nil
}

func (u *Updater) persist() error {
	u.mu.Lock()
	defer u.mu.Unlock()
	data, err := json.Marshal(u.state)
	if err != nil {
		return err
	}
	return writePrivate(filepath.Join(u.dir, ".update-state.json"), data)
}

func MarkUpdateHealthy(dir string) error {
	path := filepath.Join(dir, ".update-state.json")
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var state UpdateState
	if json.Unmarshal(data, &state) != nil {
		return errors.New("更新状态文件无效")
	}
	if state.Phase != "restarting" {
		return nil
	}
	if state.LatestVersion != Version {
		return errors.New("新程序版本与 Release 标签不一致")
	}
	state.Phase, state.Progress, state.Message, state.Available = "completed", 100, "更新完成", false
	out, _ := json.Marshal(state)
	return writePrivate(path, out)
}

func (u *Updater) MarkHealthy() error {
	if err := MarkUpdateHealthy(u.dir); err != nil {
		return err
	}
	data, err := os.ReadFile(filepath.Join(u.dir, ".update-state.json"))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	return json.Unmarshal(data, &u.state)
}

func (u *Updater) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	var err error
	switch {
	case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/status"):
	case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/source"):
		r.Body = http.MaxBytesReader(w, r.Body, 4096)
		var in updateConfig
		if json.NewDecoder(r.Body).Decode(&in) != nil {
			err = errors.New("更新源格式错误")
		} else {
			err = u.SaveSource(in.Repository, in.Token)
		}
	case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/check"):
		ctx, cancel := context.WithTimeout(r.Context(), 25*time.Second)
		defer cancel()
		_, err = u.Check(ctx)
	case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/start"):
		ctx, cancel := context.WithTimeout(r.Context(), 25*time.Second)
		defer cancel()
		err = u.Start(ctx)
	default:
		http.NotFound(w, r)
		return
	}
	if err != nil {
		_ = json.NewEncoder(w).Encode(map[string]any{"code": 400, "msg": err.Error()})
		return
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"code": 200, "data": u.Info()})
}
