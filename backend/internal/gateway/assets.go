package gateway

import (
	"embed"
	"io/fs"
	"strings"
)

//go:embed webdist/*
var webDistFS embed.FS

// GetWebAssets serves only the assets compiled into this generation. Working
// directories and neighboring projects never influence a running artifact.
func (g *OpenAIGateway) GetWebAssets() map[string][]byte {
	assets := make(map[string][]byte)
	if err := fs.WalkDir(webDistFS, "webdist", func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		content, err := webDistFS.ReadFile(path)
		if err != nil {
			return nil
		}
		// 去掉 "webdist/" 前缀，保留相对路径
		relPath := strings.TrimPrefix(path, "webdist/")
		assets[relPath] = content
		return nil
	}); err != nil && g != nil && g.logger != nil {
		g.logger.Warn("读取嵌入前端资源失败", "error", err)
	}
	return assets
}
