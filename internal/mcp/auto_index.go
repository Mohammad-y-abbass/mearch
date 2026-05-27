package mcp

import (
	"context"
	"errors"
	"log"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// autoIndexOnConnect builds the code graph when the MCP client finishes
// initialization (typically when Cursor opens a workspace and connects).
func (s *mearchServer) autoIndexOnConnect(ctx context.Context, session *sdk.ServerSession) {
	go func() {
		// The MCP initialized notification context is request-scoped and may be
		// canceled quickly after the handler returns. Use detached contexts for
		// roots lookup and indexing to avoid partial/empty indexing.
		rootsCtx, cancelRoots := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancelRoots()

		path, err := s.resolveProjectPath(rootsCtx, session)
		if err != nil {
			log.Printf("mearch: auto-index skipped: %v", err)
			return
		}
		if path == "" {
			log.Printf("mearch: auto-index skipped: no project path")
			return
		}

		s.indexMu.Lock()
		defer s.indexMu.Unlock()

		s.mu.RLock()
		already := s.g != nil && s.rootDir == path
		s.mu.RUnlock()
		if already {
			return
		}

		indexCtx, cancelIndex := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancelIndex()

		if _, err := s.indexProjectLocked(indexCtx, indexProjectArgs{Path: path, MaxDepth: 10}); err != nil {
			log.Printf("mearch: auto-index failed for %s: %v", path, err)
			return
		}
		log.Printf("mearch: auto-indexed %s", path)
	}()
}

func (s *mearchServer) resolveProjectPath(ctx context.Context, session *sdk.ServerSession) (string, error) {
	if p := strings.TrimSpace(os.Getenv("MEARCH_PROJECT_PATH")); p != "" {
		return filepath.Abs(p)
	}

	if session != nil {
		res, err := session.ListRoots(ctx, nil)
		if err == nil && len(res.Roots) > 0 {
			if p, err := rootPathFromURI(res.Roots[0].URI); err == nil {
				return p, nil
			}
		}
	}

	if wd, err := os.Getwd(); err == nil && wd != "" {
		return filepath.Abs(wd)
	}

	return "", nil
}

func rootPathFromURI(uri string) (string, error) {
	u, err := url.Parse(uri)
	if err != nil {
		return "", err
	}
	if u.Scheme != "file" {
		return "", errors.New("not a file URI")
	}
	if u.Path == "" {
		return "", errors.New("empty path")
	}
	p := filepath.Clean(filepath.FromSlash(u.Path))
	if runtime.GOOS == "windows" && len(p) >= 3 && p[2] == ':' && (p[0] == '/' || p[0] == '\\') {
		p = p[1:] // /C:/... or \C:\... -> C:\...
	}
	if !filepath.IsAbs(p) {
		return "", errors.New("not an absolute path")
	}
	return p, nil
}
