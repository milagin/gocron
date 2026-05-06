package models

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gocronx-team/gocron/internal/modules/logger"
	"github.com/ncruces/go-sqlite3/gormlite"
	"gorm.io/gorm"
)

func TestMain(m *testing.M) {
	_ = os.MkdirAll("log", 0o755)
	logger.InitLogger()
	os.Exit(m.Run())
}

// TestRepairSettings 覆盖 issue #158 触发路径：
// PG 升级到 1.5.9 时启动报 "operator does not exist: ` character varying"。
// 根因是 Where 字符串里的 MySQL 反引号 `key` PG 不识别。
// 这里用 SQLite 验证 RepairSettings 的功能正确性（空库新建/幂等/补齐缺失），
// PG 兼容性由 TestNoBacktickedIdentifiersInQuotedStrings 守住。
func TestRepairSettings(t *testing.T) {
	db, err := gorm.Open(gormlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	if err := db.AutoMigrate(&Setting{}); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	oldDb := Db
	Db = db
	defer func() { Db = oldDb }()

	t.Run("空库时创建所有必需配置", func(t *testing.T) {
		if err := RepairSettings(); err != nil {
			t.Fatalf("RepairSettings: %v", err)
		}

		var count int64
		db.Model(&Setting{}).Count(&count)
		if count == 0 {
			t.Fatal("期望创建若干配置，实际 0 条")
		}

		// 关键配置项必须存在
		mustExist := []struct{ code, key string }{
			{SlackCode, SlackUrlKey},
			{SlackCode, SlackTemplateKey},
			{MailCode, MailServerKey},
			{MailCode, MailTemplateKey},
			{WebhookCode, WebhookUrlKey},
			{WebhookCode, WebhookTemplateKey},
			{SystemCode, LogRetentionDaysKey},
			{SystemCode, LogCleanupTimeKey},
			{SystemCode, LogFileSizeLimitKey},
		}
		for _, m := range mustExist {
			var c int64
			db.Model(&Setting{}).
				Where(map[string]interface{}{"code": m.code, "key": m.key}).
				Count(&c)
			if c != 1 {
				t.Errorf("缺失配置 code=%s key=%s (count=%d)", m.code, m.key, c)
			}
		}
	})

	t.Run("幂等：重复执行不产生重复记录", func(t *testing.T) {
		var before int64
		db.Model(&Setting{}).Count(&before)

		if err := RepairSettings(); err != nil {
			t.Fatalf("RepairSettings: %v", err)
		}

		var after int64
		db.Model(&Setting{}).Count(&after)
		if before != after {
			t.Errorf("RepairSettings 不幂等：执行前 %d 条，执行后 %d 条", before, after)
		}
	})

	t.Run("部分缺失时只补齐缺失项", func(t *testing.T) {
		// 删两条特定配置
		db.Where(map[string]interface{}{"code": SystemCode, "key": LogCleanupTimeKey}).Delete(&Setting{})
		db.Where(map[string]interface{}{"code": MailCode, "key": MailServerKey}).Delete(&Setting{})

		var beforeRepair int64
		db.Model(&Setting{}).Count(&beforeRepair)

		if err := RepairSettings(); err != nil {
			t.Fatalf("RepairSettings: %v", err)
		}

		// 应补齐 2 条
		var afterRepair int64
		db.Model(&Setting{}).Count(&afterRepair)
		if afterRepair-beforeRepair != 2 {
			t.Errorf("期望补齐 2 条，实际新增 %d 条", afterRepair-beforeRepair)
		}

		// 验证补齐后这两条确实存在
		var c int64
		db.Model(&Setting{}).
			Where(map[string]interface{}{"code": SystemCode, "key": LogCleanupTimeKey}).Count(&c)
		if c != 1 {
			t.Errorf("LogCleanupTime 未补齐 (count=%d)", c)
		}
		db.Model(&Setting{}).
			Where(map[string]interface{}{"code": MailCode, "key": MailServerKey}).Count(&c)
		if c != 1 {
			t.Errorf("MailServer 未补齐 (count=%d)", c)
		}
	})
}

// TestNoBacktickedIdentifiersInQuotedStrings 是 issue #158 的静态回归守卫。
//
// 反引号 (`) 是 MySQL 专属的标识符转义符，PostgreSQL 用双引号 (")，
// 在双引号字符串里硬编码反引号会让 PG 抛 SQLSTATE 42883 启动崩溃。
//
// 此测试 AST 解析当前包的所有非测试文件，扫描双引号字符串字面量，
// 任何字符串内含反引号一律 fail —— 提示改用 map[string]interface{}{}
// 或 clause.Eq 让 GORM 按方言自动加引号。
//
// Go 原生 raw string (`...`) 不在扫描范围（Slack/Webhook 模板用的是 raw string，
// 反引号是字符串边界，不会被误报）。
func TestNoBacktickedIdentifiersInQuotedStrings(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatalf("glob: %v", err)
	}

	fset := token.NewFileSet()
	violations := 0

	for _, fname := range files {
		if strings.HasSuffix(fname, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, fname, nil, parser.ParseComments)
		if err != nil {
			t.Fatalf("parse %s: %v", fname, err)
		}

		ast.Inspect(f, func(n ast.Node) bool {
			lit, ok := n.(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				return true
			}
			// 跳过 Go raw string —— `...` 形式，反引号是边界不是内容
			if strings.HasPrefix(lit.Value, "`") {
				return true
			}
			if strings.Contains(lit.Value, "`") {
				pos := fset.Position(lit.Pos())
				t.Errorf("%s:%d 双引号字符串内含反引号 (MySQL 专属转义，PG 不兼容): %s\n"+
					"  → 改用 map[string]interface{}{...} 让 GORM 按方言加引号",
					pos.Filename, pos.Line, lit.Value)
				violations++
			}
			return true
		})
	}

	if violations > 0 {
		t.Logf("发现 %d 处反引号转义。背景：issue #158 (V1.5.9 PG 启动崩溃)", violations)
	}
}
