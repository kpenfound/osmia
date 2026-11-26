# The Claude kit writes settings as agent when creating a sandbox from this image.
install -d -o agent -g agent /home/agent/.claude
installer=$(mktemp)
trap 'rm -f "$installer"' EXIT
curl -fsSL https://dl.dagger.io/dagger/install.sh -o "$installer"
sh "$installer"
