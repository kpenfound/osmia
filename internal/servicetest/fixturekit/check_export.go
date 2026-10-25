package fixturekit

// CheckExportDagger reports the repository and files visible to a fake check
// client. It handles listing too, without any engine or network access.
const CheckExportDagger = `#!/bin/sh
set -eu
[ -z "${CI-}${DAGGER_CLOUD_TOKEN-}${GITHUB_TOKEN-}" ] || exit 90
if [ "$1" = list ]; then
  echo 'dag+check://go/test'
  exit 0
fi
[ "$*" = 'check --progress=report --fail-fast --parallel 4' ] || exit 91
if git rev-parse --verify HEAD >/dev/null 2>&1; then
  echo EXPORT_HEAD
  git rev-parse HEAD
  echo EXPORT_COMMIT
  git cat-file commit HEAD
  echo EXPORT_BRANCH
  git symbolic-ref --short HEAD
  echo EXPORT_ORIGIN
  git remote get-url origin
  echo EXPORT_TAGS
  git tag --list
  echo EXPORT_STATUS
  git status --porcelain
else
  echo EXPORT_ANONYMOUS
  [ -z "$(git remote)$(git tag --list)" ] || exit 92
fi
echo EXPORT_FILES
find . -type f ! -path './.git/*' | sort | while IFS= read -r file; do
  printf '%s\n' "$file"
  cat "$file"
done
printf '\nCHECKS  ✔ 1 passed\n✔ dag://go/test 1s OK\n'
`
