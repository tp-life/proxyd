// Package confighistory 实现本机加密配置历史；下载接口只读取独立脱敏内容。
package confighistory

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"proxyd/internal/configversion"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

// Store 将完整配置用独立本机密钥加密；互斥锁覆盖读写及保留数量清理。
type Store struct {
	dir string
	mu  sync.Mutex
}

// record 将脱敏文档与加密载荷分开编码，明文元数据不含 token、密钥或 URL 凭据。
type record struct {
	Version  configversion.Version `json:"version"`
	Redacted []byte                `json:"redacted"`
	Nonce    []byte                `json:"nonce"`
	Sealed   []byte                `json:"sealed"`
}

var validID = regexp.MustCompile(`^[0-9]{20}-[0-9a-f]{16}$`)

// New 创建惰性仓储。参数 dir 为专用目录；返回 *Store；构造不读写磁盘。
func New(dir string) *Store { return &Store{dir: dir} }

// atomicWrite 以临时文件、同步和同目录重命名发布完整文件。
// 参数 path/data 为目标与内容；返回 error；所有临时文件使用 0600，失败不覆盖原文件。
func atomicWrite(path string, data []byte) error {
	f, err := os.CreateTemp(filepath.Dir(path), ".history-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err = f.Write(data); err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	return os.Rename(f.Name(), path)
}

// cipherLocked 加载加密密钥，只在没有历史记录的初次使用时生成。
// 参数 create 指示是否允许初始化；返回 cipher.AEAD/error；密钥丢失时拒绝覆盖历史。
func (s *Store) cipherLocked(create bool) (cipher.AEAD, error) {
	keyPath := filepath.Join(s.dir, "vault.key")
	key, err := os.ReadFile(keyPath)
	if os.IsNotExist(err) && create {
		entries, readErr := os.ReadDir(s.dir)
		if readErr != nil {
			return nil, readErr
		}
		for _, entry := range entries {
			if filepath.Ext(entry.Name()) == ".json" {
				return nil, fmt.Errorf("历史密钥缺失，拒绝覆盖已有加密历史")
			}
		}
		key = make([]byte, 32)
		if _, err = rand.Read(key); err != nil {
			return nil, err
		}
		// O_EXCL 防止另一进程初始化时替换解密历史所需的密钥。
		f, writeErr := os.OpenFile(keyPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
		if writeErr != nil {
			return nil, writeErr
		}
		_, err = f.Write(key)
		if err == nil {
			err = f.Sync()
		}
		closeErr := f.Close()
		if err != nil {
			return nil, err
		}
		if closeErr != nil {
			return nil, closeErr
		}
	}
	if err != nil {
		return nil, fmt.Errorf("无法读取配置历史密钥: %w", err)
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

// Archive 保存变更前的配置，最多保留 30 个历史版本。
// 参数 raw 是完整配置，redacted 是已脱敏 YAML，reason/sections 为公开摘要；返回版本/error。
// 本机锁串行化发布，AES-GCM 绑定版本 ID 防止密文被调换；归档失败时调用者必须停止配置写入。
func (s *Store) Archive(raw, redacted []byte, reason string, sections []string) (configversion.Version, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var zero configversion.Version
	if len(raw) > 2<<20 || len(redacted) > 2<<20 {
		return zero, fmt.Errorf("历史配置超过 2 MiB")
	}
	if err := os.MkdirAll(s.dir, 0700); err != nil {
		return zero, err
	}
	aead, err := s.cipherLocked(true)
	if err != nil {
		return zero, err
	}
	random := make([]byte, 8)
	if _, err = rand.Read(random); err != nil {
		return zero, err
	}
	now := time.Now().UTC()
	version := configversion.Version{ID: strings.ReplaceAll(now.Format("20060102150405.000000"), ".", "") + "-" + hex.EncodeToString(random), CreatedAt: now, Reason: reason, Sections: sections}
	nonce := make([]byte, aead.NonceSize())
	if _, err = rand.Read(nonce); err != nil {
		return zero, err
	}
	data, err := json.Marshal(record{Version: version, Redacted: redacted, Nonce: nonce, Sealed: aead.Seal(nil, nonce, raw, []byte(version.ID))})
	if err != nil {
		return zero, err
	}
	if err = atomicWrite(filepath.Join(s.dir, version.ID+".json"), data); err != nil {
		return zero, err
	}
	versions, err := s.listLocked()
	if err == nil && len(versions) > 30 {
		for _, old := range versions[30:] {
			_ = os.Remove(filepath.Join(s.dir, old.ID+".json"))
		}
	}
	return version, nil
}

// readLocked 有界读取并验证历史 ID；参数 id 为公开版本号；返回 record/error，不允许路径穿越。
func (s *Store) readLocked(id string) (record, error) {
	var result record
	if !validID.MatchString(id) {
		return result, fmt.Errorf("历史版本 ID 无效")
	}
	f, err := os.Open(filepath.Join(s.dir, id+".json"))
	if err != nil {
		return result, fmt.Errorf("历史版本不可用")
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, 8<<20))
	if err != nil {
		return result, err
	}
	if err = json.Unmarshal(data, &result); err != nil {
		return result, fmt.Errorf("历史记录损坏")
	}
	if result.Version.ID != id {
		return result, fmt.Errorf("历史版本身份不匹配")
	}
	return result, nil
}

// listLocked 列出完整有效版本，按时间倒序；参数无；返回切片/error，目录缺失视为尚无历史。
func (s *Store) listLocked() ([]configversion.Version, error) {
	out := []configversion.Version{}
	entries, err := os.ReadDir(s.dir)
	if os.IsNotExist(err) {
		return out, nil
	}
	if err != nil {
		return nil, err
	}
	for _, entry := range entries {
		if filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		id := entry.Name()[:len(entry.Name())-5]
		r, readErr := s.readLocked(id)
		if readErr != nil {
			return nil, readErr
		}
		out = append(out, r.Version)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })
	return out, nil
}

// List 返回历史元数据；参数无；返回独立切片/error；互斥避免与保留数量清理竞争。
func (s *Store) List() ([]configversion.Version, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.listLocked()
}

// Read 在恢复用例中解密完整配置；参数 id 为版本号；返回字节/error，损坏密文不会降级返回脱敏配置。
func (s *Store) Read(id string) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, err := s.readLocked(id)
	if err != nil {
		return nil, err
	}
	aead, err := s.cipherLocked(false)
	if err != nil {
		return nil, err
	}
	if len(r.Nonce) != aead.NonceSize() {
		return nil, fmt.Errorf("历史随机数损坏")
	}
	raw, err := aead.Open(nil, r.Nonce, r.Sealed, []byte(id))
	if err != nil {
		return nil, fmt.Errorf("配置历史完整性校验失败")
	}
	return raw, nil
}

// Redacted 只返回脱敏副本；参数 id 为版本号；返回字节/error，无需解密密钥，绝不回退完整配置。
func (s *Store) Redacted(id string) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, err := s.readLocked(id)
	return r.Redacted, err
}
