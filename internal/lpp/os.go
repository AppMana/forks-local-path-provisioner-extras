package lpp

import (
	"context"
	"path/filepath"
	"regexp"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// resolveTargetOS reads the kubernetes.io/os label off the target node.
// On lookup failure the result defaults to OSLinux — most clusters are
// Linux-majority and the fallback is the safer choice.
func (p *Provisioner) resolveTargetOS(ctx context.Context, node string) string {
	if node == "" {
		return OSLinux
	}
	n, err := p.kubeClient.CoreV1().Nodes().Get(ctx, node, metav1.GetOptions{})
	if err != nil {
		return OSLinux
	}
	if v := n.Labels["kubernetes.io/os"]; v != "" {
		return v
	}
	return OSLinux
}

// pathIsAbsForOS reports whether p is an absolute path on the given OS.
// filepath.IsAbs is GOOS-specific (a Linux build of the controller will
// say "C:\\foo" isn't absolute), so we handle Windows ourselves.
var winAbsRE = regexp.MustCompile(`^[A-Za-z]:[\\/]`)

func pathIsAbsForOS(p, osType string) bool {
	if osType == OSWindows {
		return winAbsRE.MatchString(p)
	}
	return filepath.IsAbs(p)
}

// cleanPathForOS normalizes p for the given OS.
func cleanPathForOS(p, osType string) string {
	if osType == OSWindows {
		// Keep backslashes; collapse duplicates and trim trailing.
		p = strings.ReplaceAll(p, "/", "\\")
		for strings.Contains(p, "\\\\") {
			p = strings.ReplaceAll(p, "\\\\", "\\")
		}
		// Preserve drive letter; trim a trailing slash beyond "X:\".
		if len(p) > 3 && strings.HasSuffix(p, "\\") {
			p = strings.TrimRight(p, "\\")
		}
		return p
	}
	return filepath.Clean(p)
}

// splitPathForOS splits p into its directory and final element. Mirrors
// filepath.Split but honors the Windows separator on Windows paths.
func splitPathForOS(p, osType string) (dir, file string) {
	if osType == OSWindows {
		i := strings.LastIndexAny(p, "\\/")
		if i < 0 {
			return "", p
		}
		return p[:i+1], p[i+1:]
	}
	return filepath.Split(p)
}

// joinPathForOS joins parts with the OS-appropriate separator.
func joinPathForOS(osType string, parts ...string) string {
	if osType == OSWindows {
		// strings.Join then collapse double separators that arise from
		// already-terminated dir + leading-sep file parts.
		joined := strings.Join(parts, "\\")
		for strings.Contains(joined, "\\\\") {
			joined = strings.ReplaceAll(joined, "\\\\", "\\")
		}
		return joined
	}
	return filepath.Join(parts...)
}
