package doctor

import (
	"fmt"

	"github.com/heonjinjeong/forge/internal/clientver"
)

// checkClients detects the client binaries and gates their versions. This is the
// only place that runs a program, and only "<binary> --version".
func checkClients(e *env) []Check {
	var out []Check
	for _, spec := range clientver.Specs() {
		if e.ctx.Err() != nil {
			break
		}
		r := e.o.Checker.Check(e.ctx, spec)
		id := "client." + spec.Client
		title := "클라이언트 " + spec.Client
		switch r.Code {
		case clientver.CodeVerified:
			out = append(out, mk(id, "client", title, OK, fmt.Sprintf("%s %s (검증된 버전) %s", spec.Binary, r.Version, e.pathNote(r)), ""))
		case clientver.CodeUnverifiedPat:
			out = append(out, mk(id, "client", title, Info, fmt.Sprintf("%s %s: 미검증 패치 (검증된 버전 %s) %s", spec.Binary, r.Version, spec.Verified, e.pathNote(r)),
				"같은 major.minor 의 다른 patch 입니다. 설정 형식이 같을 가능성이 높지만 검증하지 않았습니다 (sync --apply 는 계속 진행)"))
		case clientver.CodeUnverifiedVer:
			out = append(out, mk(id, "client", title, Warn, fmt.Sprintf("%s %s: 미검증 버전: 생성한 설정이 올바르게 로드되지 않을 수 있습니다 (검증된 버전 %s)", spec.Binary, r.Version, spec.Verified),
				"Forge 어댑터는 "+spec.Verified+" 에서 설정 형식을 확인했습니다. 다른 major.minor 는 형식이 바뀌었을 수 있어 sync --apply 가 중단됩니다",
				"forge sync --target "+spec.Client+e.projFlag()+" --check-version   # 계획과 함께 버전 확인",
				"forge sync --target "+spec.Client+e.projFlag()+" --apply --allow-untested-version   # 위험을 감수하고 적용(백업됨)"))
		case clientver.CodeNotInstalled:
			out = append(out, mk(id, "client", title, Info, spec.Binary+" 가 PATH 에 없습니다 (설치하지 않은 클라이언트는 정상)", ""))
		case clientver.CodeUnsafeLoc:
			up := r.UnsafePath
			if up == "" {
				up = r.Path
			}
			fix := []string{"ls -ld " + e.q(up), "chmod go-w " + e.q(up) + "   # 다른 사용자 쓰기 권한 제거(소유자가 본인이 아니면 신뢰할 수 있는 위치에 다시 설치)", "which -a " + spec.Binary}
			if r.Inexact {
				fix = append(fix[:2], inexactNote, fix[2])
			}
			out = append(out, mk(id, "client", title, Warn, r.Message, "그룹/다른 사용자가 수정할 수 있는 위치의 바이너리는 실행하지 않습니다 (다른 계정이 심은 실행 파일 방지). sync --apply 도 이 경우 중단되며 --allow-untested-version 으로 우회할 수 없습니다",
				fix...))
		case clientver.CodeBlocked:
			out = append(out, mk(id, "client", title, Warn, r.Message, "프로젝트 디렉터리 안의 바이너리는 실행하지 않습니다 (저장소가 심은 실행 파일 방지)", "which -a "+spec.Binary))
		default: // run-failed, parse-failed
			out = append(out, mk(id, "client", title, Warn, r.Message, "버전을 확인하지 못했습니다. sync --apply 는 --allow-untested-version 없이는 중단됩니다",
				spec.Binary+" --version   # 직접 실행해 보세요", "which -a "+spec.Binary))
		}
	}
	return out
}

func (e *env) pathNote(r clientver.Result) string {
	if r.Path == "" {
		return ""
	}
	if r.Resolved != "" && r.Resolved != r.Path {
		return "(" + r.Path + " -> " + r.Resolved + ")"
	}
	return "(" + r.Path + ")"
}
