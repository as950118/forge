package adapter

import (
	"fmt"
	"os"

	"github.com/heonjinjeong/forge/internal/adapter/jsonedit"
	"github.com/heonjinjeong/forge/internal/textsafe"
)

// DocFile is a user-owned JSON config file loaded for in-place editing.
type DocFile struct {
	Path    string
	Exists  bool
	Orig    []byte
	Cur     []byte // working copy: Orig with the edits so far (or "{}\n" for a new file)
	Snap    FileState
	Mode    os.FileMode
	Blocked string // non-empty: the file must not be written (symlink, read-only)
}

// LoadDoc reads path (below root). A file that is not strict JSON (comments,
// trailing commas, BOM, empty) or whose top level is not an object is a hard
// error: nothing is written at all. A symlink or read-only file only blocks the
// items that need it.
func LoadDoc(root, path string, newMode os.FileMode) (*DocFile, error) {
	d := &DocFile{Path: path, Mode: newMode}
	if err := CheckNoSymlinks(root, path); err != nil {
		d.Blocked = "심볼릭 링크 경로라 따라가지 않고 거부합니다"
		d.Cur = []byte("{}\n")
		return d, nil
	}
	b, fi, ok, err := ReadFileSafe(path)
	if err != nil {
		return nil, fmt.Errorf("%s 를 읽을 수 없습니다: %s", textsafe.Display(path, 200), textsafe.Display(err.Error(), 200))
	}
	d.Snap, err = Snapshot(path)
	if err != nil {
		return nil, err
	}
	if !ok {
		d.Cur = []byte("{}\n")
		d.Blocked = WriteBlocker(path)
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
	d.Exists, d.Orig, d.Cur, d.Mode = true, b, append([]byte(nil), b...), fi.Mode().Perm()
	d.Blocked = WriteBlocker(path)
	return d, nil
}

// Op returns the write operation, or false when nothing changed.
func (d *DocFile) Op() (Op, bool) {
	if d.Blocked != "" || (d.Exists && string(d.Cur) == string(d.Orig)) || (!d.Exists && string(d.Cur) == "{}\n") {
		return Op{}, false
	}
	return Op{Kind: OpWriteFile, Path: d.Path, Data: d.Cur, Mode: d.Mode, Before: d.Snap, BeforeData: d.Orig}, true
}
