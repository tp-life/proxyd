// Package configversion 定义配置版本聚合及仓储接口，不依赖文件系统或配置解析器。
package configversion

import "time"

// Version 是可公开展示的历史元数据；内容摘要不对外暴露完整凭据。
type Version struct {
	ID        string    `json:"id"`
	CreatedAt time.Time `json:"created_at"`
	Reason    string    `json:"reason"`
	Sections  []string  `json:"sections"`
}

// Repository 是配置历史的持久化端口；完整配置只能由应用层在恢复用例中读取。
type Repository interface {
	Archive(raw []byte, redacted []byte, reason string, sections []string) (Version, error)
	List() ([]Version, error)
	Read(id string) ([]byte, error)
	Redacted(id string) ([]byte, error)
}
