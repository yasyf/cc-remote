package images

import "embed"

//go:embed artifacts.sh provision.sh plugins.sh start.sh supervise.py capture.py loader.py namespace/Dockerfile namespace/finalize.sh
var FS embed.FS
