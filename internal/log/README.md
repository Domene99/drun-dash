# internal/log

Structured-logging setup shared by every drun-dash binary (`control-plane`,
`llm-proxy`, and the `drun-dash` CLI).

[`New`](./log.go) builds the standard-library [`*slog.Logger`](https://pkg.go.dev/log/slog)
from a [`*config.Config`](../config) so all binaries log identically:

- **Handler** — JSON in production (`cfg.IsProduction()`), human-readable text
  everywhere else.
- **Level** — driven by `cfg.LogLevel` (`debug`, `info`, `warn`, `error`);
  any unrecognized value falls back to `info`.
- **Output** — `os.Stdout`.

`New` deliberately does **not** call `slog.SetDefault`. Whether to install the
logger as the process default is left to each binary's `main`.

## Usage

```go
import (
	"log/slog"

	"github.com/Domene99/drun-dash/internal/config"
	"github.com/Domene99/drun-dash/internal/httpx"
	logpkg "github.com/Domene99/drun-dash/internal/log"
)

cfg, err := config.Load()
if err != nil {
	// handle error
}

logger := logpkg.New(cfg)
slog.SetDefault(logger) // optional: main decides

// Feed the logger into the HTTP middleware.
recoverer := httpx.Recoverer(logger)
requestLogger := httpx.Logger(logger)
```
