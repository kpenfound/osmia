installer=$(mktemp)
trap 'rm -f "$installer"' EXIT
curl -fsSL https://downloads.claude.ai/claude-code-releases/bootstrap.sh -o "$installer"
bash "$installer"
