package graphdriver

import (
	"os"
	"path/filepath"
	"testing"

	"gotest.tools/v3/assert"
)

func TestIsEmptyDir(t *testing.T) {
	tmp, err := os.MkdirTemp("", "test-is-empty-dir")
	assert.NilError(t, err)
	defer os.RemoveAll(tmp)

	d := filepath.Join(tmp, "empty-dir")
	err = os.Mkdir(d, 0o755)
	assert.NilError(t, err)
	empty := isEmptyDir(d)
	assert.Check(t, empty)

	d = filepath.Join(tmp, "dir-with-subdir")
	err = os.MkdirAll(filepath.Join(d, "subdir"), 0o755)
	assert.NilError(t, err)
	empty = isEmptyDir(d)
	assert.Check(t, !empty)

	d = filepath.Join(tmp, "dir-with-empty-file")
	err = os.Mkdir(d, 0o755)
	assert.NilError(t, err)
	f, err := os.CreateTemp(d, "file")
	assert.NilError(t, err)
	defer f.Close()
	empty = isEmptyDir(d)
	assert.Check(t, !empty)
}
