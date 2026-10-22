package opencode

import (
	"fmt"
	"os"

	"github.com/heonjinjeong/forge/internal/adapter"
	"github.com/heonjinjeong/forge/internal/adapter/jsonedit"
	"github.com/heonjinjeong/forge/internal/textsafe"
)

// docFile is a user-owned JSON config file loaded for in-place editing.
type docFile struct {
	path    string
	exists  bool
	orig    []byte
	cur     []byte // working copy: orig with the edits so far (or "{}\n" for a new file)
	snap    adapter.FileState
	mode    os.FileMode
	blocked string // non-empty: the file must not be written (symlink, read-only)
}

// loadDoc reads path. A file that is not strict JSON (comments, trailing commas,
// BOM, empty) or whose top level is not an object is a hard error: nothing is
// written at all. A symlink or read-only file only blocks the items that need it.
func loadDoc(root, path string, newMode os.FileMode) (*docFile, error) {
	d := &docFile{path: path, mode: newMode}
	if err := adapter.CheckNoSymlinks(root, path); err != nil {
		d.blocked = "심볼릭 링크 경로라 따라가지 않고 거부합니다"
		d.cur = []byte("{}\n")
		return d, nil
	}
	b, fi, ok, err := adapter.ReadFileSafe(path)
	if err != nil {
		return nil, fmt.Errorf("%s 를 읽을 수 없습니다: %s", textsafe.Display(path, 200), textsafe.Display(err.Error(), 200))
	}
	d.snap, err = adapter.Snapshot(path)
	if err != nil {
		return nil, err
	}
	if !ok {
		d.cur = []byte("{}\n")
		d.blocked = adapter.WriteBlocker(path)
		return d, nil
	}
	root0, err := jsonedit.Parse(b)
	if err != nil {
		return nil, fmt.Errorf("%s 를 JSON 으로 해석할 수 없어 아무것도 쓰지 않고 중단합니다: %s. 파일을 고친 뒤 다시 실행하세요",
			textsafe.Display(path, 200), textsafe.Display(err.Error(), 200))
	}
	if root0.Kind != jsonedit.Object {
		return nil, fmt.Errorf("%s 의 최상위 값이 JSON 객체가 아니라 아무것도 쓰지 않고 중단합니다", textsafe.Display(path, 200))
	}
	d.exists, d.orig, d.cur, d.mode = true, b, append([]byte(nil), b...), fi.Mode().Perm()
	d.blocked = adapter.WriteBlocker(path)
	return d, nil
}

// op returns the write operation, or false when nothing changed.
func (d *docFile) op() (adapter.Op, bool) {
	if d.blocked != "" || (d.exists && string(d.cur) == string(d.orig)) || (!d.exists && string(d.cur) == "{}\n") {
		return adapter.Op{}, false
	}
	return adapter.Op{Kind: adapter.OpWriteFile, Path: d.path, Data: d.cur, Mode: d.mode, Before: d.snap, BeforeData: d.orig}, true
}
