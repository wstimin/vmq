package maintenance

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	drivermysql "github.com/go-sql-driver/mysql"
	store "github.com/hulisang/vmqfox-backend/internal/adapter/mysql"
	"github.com/hulisang/vmqfox-backend/internal/auth"
	gormmysql "gorm.io/driver/mysql"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func environment(in InstallInput, secret string, port string) []byte {
	values := [][2]string{
		{"VMQ_SERVER_HOST", "0.0.0.0"}, {"VMQ_SERVER_PORT", port}, {"VMQ_SERVER_MODE", "release"},
		{"VMQ_FRONTEND_URL", in.SiteURL}, {"VMQ_ALLOWED_ORIGIN", in.SiteURL}, {"VMQ_RUNTIME_MODE", "writer"},
		{"VMQ_DB_HOST", in.Host}, {"VMQ_DB_PORT", strconv.Itoa(in.Port)}, {"VMQ_DB_NAME", in.Database},
		{"VMQ_DB_USER", in.User}, {"VMQ_DB_PASSWORD", in.Password}, {"VMQ_DB_CHARSET", "utf8mb4"},
		{"VMQ_TOKEN_SECRET", secret}, {"VMQ_TOKEN_ISSUER", "vmqfox"}, {"VMQ_TOKEN_TTL", "24h"},
	}
	var result strings.Builder
	for _, item := range values {
		fmt.Fprintf(&result, "%s=\"%s\"\n", item[0], item[1])
	}
	return []byte(result.String())
}

func initializeDatabase(ctx context.Context, in InstallInput, dir, installToken string) error {
	if err := in.validate(); err != nil {
		return err
	}
	needed, err := needsInstall(dir)
	if err != nil || !needed {
		return errors.New("安装已锁定：存在配置文件或安装标记，禁止重新安装")
	}
	lockPath := filepath.Join(dir, ".installing")
	if err := os.Mkdir(lockPath, 0700); err != nil {
		return errors.New("另一安装任务正在运行，或上次安装中断；请先检查服务日志")
	}
	defer os.Remove(lockPath)
	jwtSecret, err := randomSecret()
	if err != nil {
		return err
	}
	communicationKey, err := randomSecret()
	if err != nil {
		return err
	}
	hash, err := auth.NewBcryptPasswordHasher().Hash(in.AdminPassword)
	if err != nil {
		return errors.New("管理员密码不符合要求")
	}
	cfg := drivermysql.NewConfig()
	cfg.User, cfg.Passwd, cfg.DBName = in.User, in.Password, in.Database
	cfg.Net, cfg.Addr = "tcp", net.JoinHostPort(in.Host, strconv.Itoa(in.Port))
	cfg.ParseTime, cfg.AllowNativePasswords = true, true
	cfg.Params = map[string]string{"charset": "utf8mb4"}
	cfg.Timeout, cfg.ReadTimeout, cfg.WriteTimeout = 5*time.Second, 15*time.Second, 15*time.Second
	db, err := sql.Open("mysql", cfg.FormatDSN())
	if err != nil {
		return errors.New("数据库连接参数无效")
	}
	defer func() { _ = db.Close() }()
	if err = db.PingContext(ctx); err != nil {
		var mysqlErr *drivermysql.MySQLError
		if !errors.As(err, &mysqlErr) || mysqlErr.Number != 1049 {
			return databaseError(err)
		}
		_ = db.Close()
		cfg.DBName = ""
		bootstrap, openErr := sql.Open("mysql", cfg.FormatDSN())
		if openErr != nil {
			return databaseError(openErr)
		}
		_, createErr := bootstrap.ExecContext(ctx, "CREATE DATABASE IF NOT EXISTS `"+in.Database+"` CHARACTER SET utf8mb4 COLLATE utf8mb4_unicode_ci")
		_ = bootstrap.Close()
		if createErr != nil {
			return errors.New("数据库不存在且当前账号无法创建，请先在 1Panel 创建空数据库，或使用有建库权限的账号")
		}
		cfg.DBName = in.Database
		db, err = sql.Open("mysql", cfg.FormatDSN())
		if err != nil {
			return databaseError(err)
		}
		if err = db.PingContext(ctx); err != nil {
			return databaseError(err)
		}
	}
	db.SetMaxOpenConns(4)
	db.SetMaxIdleConns(2)
	lockConn, err := db.Conn(ctx)
	if err != nil {
		return databaseError(err)
	}
	defer lockConn.Close()
	ownerSum := sha256.Sum256([]byte(installToken))
	owner := hex.EncodeToString(ownerSum[:])
	var acquired sql.NullInt64
	lockName := "vmqfox-install:" + in.Database
	if len(lockName) > 64 {
		databaseSum := sha256.Sum256([]byte(in.Database))
		lockName = "vmqfox-install:" + hex.EncodeToString(databaseSum[:16])
	}
	if err := lockConn.QueryRowContext(ctx, "SELECT GET_LOCK(?, 0)", lockName).Scan(&acquired); err != nil || !acquired.Valid || acquired.Int64 != 1 {
		return errors.New("该数据库正在被另一安装任务使用，请稍后再试")
	}
	defer func() { _, _ = lockConn.ExecContext(context.Background(), "DO RELEASE_LOCK(?)", lockName) }()
	var tables int
	if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM information_schema.tables WHERE table_schema = ?", in.Database).Scan(&tables); err != nil {
		return databaseError(err)
	}
	if tables > 0 {
		var previousOwner string
		if err := db.QueryRowContext(ctx, "SELECT setting_value FROM settings WHERE setting_key = '_install_owner'").Scan(&previousOwner); err != nil || previousOwner != owner {
			return errors.New("该数据库已有数据。为防止覆盖，请使用空数据库，不要使用现有业务库")
		}
	}
	orm, err := gorm.Open(gormmysql.New(gormmysql.Config{Conn: db}), &gorm.Config{Logger: logger.Discard})
	if err != nil {
		return databaseError(err)
	}
	if err := store.InitSchema(orm.WithContext(ctx)); err != nil {
		return errors.New("创建数据表失败，请确认数据库账号拥有建表、索引权限，并使用 MariaDB 10.11 或兼容版本")
	}
	if _, err := db.ExecContext(ctx, "INSERT INTO settings (setting_key,setting_value) VALUES ('_install_owner',?) ON DUPLICATE KEY UPDATE setting_value=VALUES(setting_value)", owner); err != nil {
		return databaseError(err)
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return databaseError(err)
	}
	defer tx.Rollback()
	var admins int
	if err := tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM admin_credentials").Scan(&admins); err != nil {
		return databaseError(err)
	}
	if admins != 0 {
		return errors.New("数据库已存在管理员，拒绝覆盖。请使用原配置启动或更换空数据库")
	}
	now := time.Now().UTC()
	if _, err := tx.ExecContext(ctx, "INSERT INTO admin_credentials (id,username,password_hash,enabled,created_at,updated_at) VALUES (1,?,?,1,?,?)", in.AdminUser, hash, now, now); err != nil {
		return errors.New("管理员创建失败，原有账号未被覆盖")
	}
	if _, err := tx.ExecContext(ctx, "UPDATE settings SET setting_value=? WHERE setting_key='key'", communicationKey); err != nil {
		return databaseError(err)
	}
	port := os.Getenv("VMQ_SERVER_PORT")
	if port == "" {
		port = "8080"
	}
	configPath := filepath.Join(dir, ".env")
	if err := writeNewPrivate(configPath, environment(in, jwtSecret, port)); err != nil {
		return errors.New("保存配置失败：目录不可写或已有 .env；管理员修改已取消")
	}
	if err := tx.Commit(); err != nil {
		_ = os.Remove(configPath)
		return errors.New("数据库提交失败，配置已撤回，请检查数据库状态后重试")
	}
	// The configuration itself also locks installation if writing the extra marker fails.
	_ = writePrivate(filepath.Join(dir, ".installed"), []byte(time.Now().UTC().Format(time.RFC3339)))
	return nil
}

func databaseError(err error) error {
	var mysqlErr *drivermysql.MySQLError
	if errors.As(err, &mysqlErr) {
		return fmt.Errorf("数据库连接或操作失败（错误码 %d），请检查地址、账号、密码和权限", mysqlErr.Number)
	}
	return errors.New("无法连接数据库，请检查数据库运行状态、宿主机地址和端口映射")
}
