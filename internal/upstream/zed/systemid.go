package zed

// ─────────────────── 本机 Zed 安装标识（system_id）读取 ───────────────────
//
// Zed 的试用额度绑定真实安装：system_id 是首次启动时生成的 UUID，存在 Zed 自己的
// SQLite 数据库（db/0-global/db.sqlite 的 kv_store 表）里。本项目零外部依赖，
// 这里手写一个只读的最小 SQLite 解析器（文件头 + sqlite_master + 表 b-tree +
// 记录 varint 头），只为取出这一个键。best-effort：读不到就省略 x-zed-system-id
// 头，由上游决定账号是否有试用权限，绝不伪造随机值。
//
// 已知局限：Zed 正在运行时 WAL 里可能有未合并数据，本解析器只读主文件；
// system_id 只在首启写入、通常早已 checkpoint，实际影响可忽略。

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// zedSystemIDDBPaths 本机 Zed 全局库的候选路径（按平台）
func zedSystemIDDBPaths() []string {
	switch runtime.GOOS {
	case "windows":
		if d := strings.TrimSpace(os.Getenv("LOCALAPPDATA")); d != "" {
			return []string{filepath.Join(d, "Zed", "db", "0-global", "db.sqlite")}
		}
	case "darwin":
		if home, err := os.UserHomeDir(); err == nil && home != "" {
			return []string{filepath.Join(home, "Library", "Application Support", "Zed", "db", "0-global", "db.sqlite")}
		}
	default:
		if d := strings.TrimSpace(os.Getenv("XDG_DATA_HOME")); d != "" {
			return []string{filepath.Join(d, "zed", "db", "0-global", "db.sqlite")}
		}
		if home, err := os.UserHomeDir(); err == nil && home != "" {
			return []string{filepath.Join(home, ".local", "share", "zed", "db", "0-global", "db.sqlite")}
		}
	}
	return nil
}

// zedReadLocalSystemID 从本机 Zed 安装读取 system_id。没有安装/读不到时返回空串，
// 不视为错误（登录降级继续）；文件存在但损坏同样降级，只留一条 WARN。
func zedReadLocalSystemID() string {
	for _, path := range zedSystemIDDBPaths() {
		if _, err := os.Stat(path); err != nil {
			continue
		}
		id, err := zedSQLiteKV(path, "system_id")
		if err != nil {
			log.Printf("[WARN] 读取本机 Zed 数据库 %s 失败（忽略 system_id）: %v", path, err)
			return ""
		}
		if id == "" {
			return ""
		}
		if norm, err := zedNormalizeSystemID(id); err == nil && norm != "" {
			return norm
		}
		log.Printf("[WARN] 本机 Zed 数据库里的 system_id 不是合法 UUID（忽略）")
		return ""
	}
	return ""
}

// zedNormalizeSystemID 校验并规范 UUID 形态（8-4-4-4-12，十六进制，非全零）
func zedNormalizeSystemID(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", nil
	}
	if len(raw) != 36 || raw[8] != '-' || raw[13] != '-' || raw[18] != '-' || raw[23] != '-' {
		return "", errors.New("system_id 必须是 UUID（8-4-4-4-12）")
	}
	for _, c := range raw {
		switch {
		case c == '-':
		case c >= '0' && c <= '9', c >= 'a' && c <= 'f', c >= 'A' && c <= 'F':
		default:
			return "", errors.New("system_id 含非十六进制字符")
		}
	}
	lower := strings.ToLower(raw)
	if lower == "00000000-0000-0000-0000-000000000000" {
		return "", errors.New("system_id 不能是全零 UUID")
	}
	return lower, nil
}

// zedResolveSystemID 安装标识的取值链：表单值 → 环境变量 → 本机 Zed 数据库。
// 表单显式给了非法值直接报错（用户输入要给反馈）；其余环节失败降级为空。
func zedResolveSystemID(form string) (string, error) {
	id, err := zedNormalizeSystemID(form)
	if err != nil {
		return "", err
	}
	if id != "" {
		return id, nil
	}
	if raw := strings.TrimSpace(os.Getenv(zedSystemIDEnv)); raw != "" {
		id, err := zedNormalizeSystemID(raw)
		if err != nil {
			log.Printf("[WARN] 环境变量 %s 非法（%v），忽略", zedSystemIDEnv, err)
		} else if id != "" {
			return id, nil
		}
	}
	return zedReadLocalSystemID(), nil
}

// ── 只读 SQLite 最小解析器 ────────────────────────────────────────────────

type zedSQLite struct {
	data     []byte
	pageSize int
	usable   int // 页大小减去每页保留字节
}

// zedSQLiteKV 读取表 kv_store 中 key 对应的文本值（找不到返回空串）
func zedSQLiteKV(path, key string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	db, err := zedParseSQLite(data)
	if err != nil {
		return "", err
	}
	root, err := db.tableRoot("kv_store")
	if err != nil {
		return "", err
	}
	var found string
	db.walkTable(root, func(cols [][]byte) bool {
		if len(cols) >= 2 && string(cols[0]) == key && len(cols[1]) > 0 {
			found = string(cols[1])
			return true
		}
		return false
	})
	return found, nil
}

func zedParseSQLite(data []byte) (db *zedSQLite, err error) {
	defer func() { // 损坏文件不该把网关带崩：任何越界都收敛成解析错误
		if r := recover(); r != nil {
			err = fmt.Errorf("SQLite 解析异常: %v", r)
		}
	}()
	if len(data) < 512 || !bytes.HasPrefix(data, []byte("SQLite format 3\x00")) {
		return nil, errors.New("不是 SQLite 数据库文件")
	}
	ps := int(binary.BigEndian.Uint16(data[16:18]))
	if ps == 1 {
		ps = 65536
	}
	if ps < 512 || ps&(ps-1) != 0 {
		return nil, errors.New("SQLite 页大小非法")
	}
	return &zedSQLite{data: data, pageSize: ps, usable: ps - int(data[20])}, nil
}

func (d *zedSQLite) page(n int) ([]byte, error) {
	if n < 1 || n*d.pageSize > len(d.data) {
		return nil, fmt.Errorf("页号越界: %d", n)
	}
	return d.data[(n-1)*d.pageSize : n*d.pageSize], nil
}

// tableRoot 从 sqlite_master（根在页 1）找表的根页号
func (d *zedSQLite) tableRoot(name string) (int, error) {
	var root int
	d.walkTable(1, func(cols [][]byte) bool {
		// sqlite_master 列序：type, name, tbl_name, rootpage, sql
		if len(cols) >= 4 && string(cols[0]) == "table" && string(cols[1]) == name {
			root = int(zedBEInt(cols[3]))
			return true
		}
		return false
	})
	if root == 0 {
		return 0, fmt.Errorf("表 %s 不存在", name)
	}
	return root, nil
}

// walkTable 遍历表 b-tree 的所有记录（visit 返回 true 提前终止）
func (d *zedSQLite) walkTable(pageNo int, visit func(cols [][]byte) bool) {
	pg, err := d.page(pageNo)
	if err != nil {
		return
	}
	off := 0
	if pageNo == 1 {
		off = 100 // 页 1 前部是数据库文件头
	}
	if off+8 > len(pg) {
		return
	}
	typ := pg[off]
	ncell := int(binary.BigEndian.Uint16(pg[off+3 : off+5]))
	switch typ {
	case 0x0d: // 叶子表页：单元格 = payload 长度 varint + rowid varint + 记录
		ptrs := off + 8
		if ptrs+ncell*2 > len(pg) {
			return
		}
		for i := 0; i < ncell; i++ {
			cp := int(binary.BigEndian.Uint16(pg[ptrs+i*2 : ptrs+i*2+2]))
			if cp < 0 || cp >= len(pg) {
				continue
			}
			cell := pg[cp:]
			plen, n := zedVarint(cell)
			_, n2 := zedVarint(cell[n:])
			start := cp + n + n2
			end := start + int(plen)
			if plen < 0 || end > len(pg) || int(plen) > d.usable-35 {
				continue // 溢出页/越界记录超出本解析器范围，跳过
			}
			if visit(zedRecordColumns(pg[start:end])) {
				return
			}
		}
	case 0x05: // 内部表页：单元格 = 左子页指针 + rowid；另有最右子页指针
		ptrs := off + 12
		if ptrs+ncell*2 <= len(pg) {
			for i := 0; i < ncell; i++ {
				cp := int(binary.BigEndian.Uint16(pg[ptrs+i*2 : ptrs+i*2+2]))
				if cp < 0 || cp+4 > len(pg) {
					continue
				}
				d.walkTable(int(binary.BigEndian.Uint32(pg[cp:cp+4])), visit)
			}
		}
		if off+12 <= len(pg) {
			d.walkTable(int(binary.BigEndian.Uint32(pg[off+8:off+12])), visit)
		}
	}
}

// zedRecordColumns 解码记录：头长度 varint + 各列 serial type varint + 值区。
// 只保留字节形态的列（文本/二进制/int），NULL/常量列为 nil。
func zedRecordColumns(payload []byte) [][]byte {
	hlen, n := zedVarint(payload)
	if int(hlen) > len(payload) || n <= 0 {
		return nil
	}
	var types []int64
	for i := n; i < int(hlen); {
		t, m := zedVarint(payload[i:])
		if m <= 0 {
			break
		}
		types = append(types, t)
		i += m
	}
	var cols [][]byte
	off := int(hlen)
	for _, t := range types {
		var size int
		switch {
		case t == 0, t == 8, t == 9: // NULL / 0 / 1 常量
			cols = append(cols, nil)
			continue
		case t >= 1 && t <= 6:
			size = map[int64]int{1: 1, 2: 2, 3: 3, 4: 4, 5: 6, 6: 8}[t]
		case t == 7:
			size = 8
		case t >= 12 && t%2 == 0:
			size = int((t - 12) / 2)
		case t >= 13:
			size = int((t - 13) / 2)
		default:
			return nil
		}
		if off+size > len(payload) {
			return nil
		}
		cols = append(cols, payload[off:off+size])
		off += size
	}
	return cols
}

// zedVarint SQLite 变长整数：大端 7bit 一段，至多 9 字节（第 9 字节满 8bit）
func zedVarint(b []byte) (int64, int) {
	var v uint64
	for i := 0; i < 8; i++ {
		if i >= len(b) {
			return int64(v), i
		}
		v = v<<7 | uint64(b[i]&0x7f)
		if b[i]&0x80 == 0 {
			return int64(v), i + 1
		}
	}
	if len(b) >= 9 {
		v = v<<8 | uint64(b[8])
		return int64(v), 9
	}
	return int64(v), len(b)
}

// zedBEInt 大端整数 → int64（1/2/3/4/6/8 字节）
func zedBEInt(b []byte) int64 {
	var v int64
	for _, c := range b {
		v = v<<8 | int64(c)
	}
	return v
}
