#!/usr/bin/env bash
# 构建 nec8mon 的 Windows 发行包。
#
#   ./deploy/build.sh
#
# 产物输出到 dist/：
#   nec8mon.exe        主程序（单文件，无运行时依赖）
#   nec8mon-386.exe    32 位版本（老机器备用）
#   install.ps1        一键安装脚本
#   uninstall.ps1      干净卸载脚本
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"

GO="${GO:-go}"
command -v "$GO" >/dev/null 2>&1 || {
  echo "找不到 go，请先安装 Go 1.21+ 或设置 GO=/path/to/go" >&2
  exit 1
}

echo "==> go vet"
"$GO" vet ./...

# Windows 专用的代码在当前平台上不会被编译，单独再查一遍
echo "==> go vet (windows/amd64)"
GOOS=windows GOARCH=amd64 "$GO" vet ./...

echo "==> go test"
"$GO" test ./...

mkdir -p dist

echo "==> 构建 windows/amd64"
GOOS=windows GOARCH=amd64 CGO_ENABLED=0 \
  "$GO" build -trimpath -ldflags="-s -w" -o dist/nec8mon.exe .

echo "==> 构建 windows/386"
GOOS=windows GOARCH=386 CGO_ENABLED=0 \
  "$GO" build -trimpath -ldflags="-s -w" -o dist/nec8mon-386.exe .

echo "==> 拷贝部署脚本"
cp deploy/install.ps1 deploy/uninstall.ps1 deploy/README.txt dist/
cp assets/NOTICE.txt dist/THIRD-PARTY-NOTICE.txt

# Windows PowerShell 5.1 只有读到 UTF-8 BOM 才会按 UTF-8 解码脚本，否则中文会变乱码
add_bom() {
  local f="$1"
  if ! head -c 3 "$f" | od -An -tx1 | grep -q 'ef bb bf'; then
    printf '\xEF\xBB\xBF' > "$f.bom" && cat "$f" >> "$f.bom" && mv "$f.bom" "$f"
    echo "    已为 $f 添加 UTF-8 BOM"
  fi
}
add_bom dist/install.ps1
add_bom dist/uninstall.ps1
add_bom dist/README.txt

echo "==> 生成校验和"
( cd dist && shasum -a 256 *.exe > SHA256SUMS.txt 2>/dev/null || sha256sum *.exe > SHA256SUMS.txt )

ls -la dist
echo
echo "完成。把整个 dist/ 目录复制到 NEC8，右键以管理员身份运行 install.ps1。"
