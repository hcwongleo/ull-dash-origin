package server

import "testing"

// resetFiles clears the package-level file map between tests. The map is global
// state in upstream, so tests that do not reset it see each other's files.
func resetFiles(t *testing.T) {
	t.Helper()
	FilesLock.Lock()
	Files = map[string]*File{}
	FilesLock.Unlock()
}
