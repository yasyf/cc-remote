package images

import "embed"

//go:embed artifacts.sh provision.sh plugins.sh start.sh supervise.py namespace/Dockerfile
var FS embed.FS
