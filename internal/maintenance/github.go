package maintenance

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"time"
)

var repositoryPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]*/[A-Za-z0-9][A-Za-z0-9_.-]*$`)
var versionPattern = regexp.MustCompile(`^[vV]?(\d+)\.(\d+)\.(\d+)(?:-web\.(\d+))?$`)

func normalizeRepository(value string) (string, error) {
	value = strings.TrimSuffix(strings.TrimSuffix(strings.TrimSpace(value), "/"), ".git")
	value = strings.TrimPrefix(value, "https://github.com/")
	if !repositoryPattern.MatchString(value) {
		return "", errors.New("请填写 owner/repository 或 https://github.com/owner/repository")
	}
	return value, nil
}

func newer(candidate, current string) bool {
	a, b := versionPattern.FindStringSubmatch(candidate), versionPattern.FindStringSubmatch(current)
	if a == nil || b == nil {
		return false
	}
	for i := 1; i <= 3; i++ {
		x, errX := strconv.ParseUint(a[i], 10, 32)
		y, errY := strconv.ParseUint(b[i], 10, 32)
		if errX != nil || errY != nil {
			return false
		}
		if x != y {
			return x > y
		}
	}
	if a[4] == "" {
		return b[4] != ""
	}
	if b[4] == "" {
		return false
	}
	x, errX := strconv.ParseUint(a[4], 10, 32)
	y, errY := strconv.ParseUint(b[4], 10, 32)
	return errX == nil && errY == nil && x > y
}

type releaseAsset struct {
	ID     int64  `json:"id"`
	Name   string `json:"name"`
	Size   int64  `json:"size"`
	Digest string `json:"digest"`
}

type release struct {
	Tag        string         `json:"tag_name"`
	Draft      bool           `json:"draft"`
	Prerelease bool           `json:"prerelease"`
	Assets     []releaseAsset `json:"assets"`
}

func githubClient() *http.Client {
	return &http.Client{Timeout: 10 * time.Minute, CheckRedirect: func(req *http.Request, via []*http.Request) error {
		if len(via) >= 5 || req.URL.Scheme != "https" {
			return errors.New("更新下载重定向无效")
		}
		host := strings.ToLower(req.URL.Hostname())
		if host != "api.github.com" && host != "github.com" && !strings.HasSuffix(host, ".githubusercontent.com") {
			return errors.New("更新下载只允许 GitHub 官方地址")
		}
		if host != "api.github.com" {
			req.Header.Del("Authorization")
		}
		return nil
	}}
}

func (u *Updater) github(ctx context.Context, path, accept string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://api.github.com"+path, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", accept)
	req.Header.Set("User-Agent", "VMQFox-WebUpdater/"+Version)
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	if u.config.Token != "" {
		req.Header.Set("Authorization", "Bearer "+u.config.Token)
	}
	response, err := u.client.Do(req)
	if err != nil {
		return nil, errors.New("无法连接 GitHub，请检查服务器网络")
	}
	if response.StatusCode != http.StatusOK {
		response.Body.Close()
		return nil, fmt.Errorf("GitHub 返回 %d：请检查仓库、Release、访问令牌或 API 频率限制", response.StatusCode)
	}
	return response, nil
}

func (u *Updater) latest(ctx context.Context) (releaseAsset, string, string, error) {
	response, err := u.github(ctx, "/repos/"+u.config.Repository+"/releases/latest", "application/vnd.github+json")
	if err != nil {
		return releaseAsset{}, "", "", err
	}
	defer response.Body.Close()
	data, err := io.ReadAll(io.LimitReader(response.Body, (2<<20)+1))
	if err != nil || len(data) > 2<<20 {
		return releaseAsset{}, "", "", errors.New("GitHub 版本信息过大或读取失败")
	}
	var rel release
	if json.Unmarshal(data, &rel) != nil || rel.Draft || rel.Prerelease || versionPattern.FindStringSubmatch(rel.Tag) == nil {
		return releaseAsset{}, "", "", errors.New("Release 版本无效，请使用 v3.1.0-web.2 或 v3.1.1 这样的正式版本号")
	}
	assetName := "vmqfox-api-linux-" + runtime.GOARCH
	var binary, checksums releaseAsset
	for _, asset := range rel.Assets {
		if asset.Name == assetName {
			binary = asset
		}
		if asset.Name == "checksums.txt" {
			checksums = asset
		}
	}
	if binary.ID <= 0 || binary.Size < 64 || binary.Size > 256<<20 {
		return releaseAsset{}, "", "", fmt.Errorf("Release 缺少有效的 %s 附件，请使用改造版发布工作流", assetName)
	}
	digest := strings.TrimPrefix(binary.Digest, "sha256:")
	if binary.Digest == "" && checksums.ID > 0 {
		r, err := u.github(ctx, "/repos/"+u.config.Repository+"/releases/assets/"+strconv.FormatInt(checksums.ID, 10), "application/octet-stream")
		if err != nil {
			return releaseAsset{}, "", "", err
		}
		text, readErr := io.ReadAll(io.LimitReader(r.Body, 64<<10))
		r.Body.Close()
		if readErr != nil {
			return releaseAsset{}, "", "", errors.New("无法读取更新校验文件")
		}
		for _, line := range strings.Split(string(text), "\n") {
			fields := strings.Fields(line)
			if len(fields) == 2 && strings.TrimPrefix(fields[1], "*") == assetName {
				digest = fields[0]
				break
			}
		}
	}
	decoded, err := hex.DecodeString(digest)
	if err != nil || len(decoded) != sha256.Size {
		return releaseAsset{}, "", "", errors.New("更新附件缺少 SHA-256 校验，请重新使用本项目工作流发布")
	}
	return binary, rel.Tag, strings.ToLower(digest), nil
}

func releaseURL(repository, tag string) string {
	return "https://github.com/" + repository + "/releases/tag/" + url.PathEscape(tag)
}
