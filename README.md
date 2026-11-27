# codist-model

The parts of [Codist](https://github.com/abxengg/codist) that deal with text from outside the code:

- the root package talks to the model server the user runs: it checks the server has the model and asks for JSON
  that follows a schema, streaming the reply. Codist contains no model of its own. It speaks Ollama's API
  (`/api/tags`, `/api/chat`) and the OpenAI-style API of LM Studio, llama.cpp's server, vLLM and Jan
  (`/v1/models`, `/v1/chat/completions`), and finds out which by asking; `CODIST_MODEL_API` or an address ending in
  `/v1` decides it.
- three timers end a call that will not finish: no first word in 3 minutes (`CODIST_FIRST_WORD`, seconds; one call
  had held Codist's review lane for 17 minutes, while over 288 calls the first word came after 63 s at most), 30
  seconds of silence once the model is writing, and 12 minutes in all. A reply that turns into blank space is ended
  too.
- `incident` reads production logs: it finds the Java errors, groups repeats, and keeps both the innermost cause's
  stack frames and the outer exception's.

```go
import "github.com/abxengg/codist-model"
import "github.com/abxengg/codist-model/incident"

client := model.New("http://localhost:11434", "qwen3.5:9b", 8192)
ok, note := client.Status(ctx)
errs := incident.Parse(logs)
```

```bash
go test ./...
```

## Licence

Source available under the [PolyForm Noncommercial License 1.0.0](LICENSE): free for any noncommercial purpose, such
as personal study, research, hobby projects and testing, and for use by charities, schools, public research and
public health bodies and government institutions. **Using it for a business, or in anything sold, needs a commercial
licence** from the author: open an issue at [github.com/abxengg/codist](https://github.com/abxengg/codist/issues)
to ask. Third-party code included in Codist keeps its own licence.

Required Notice: Copyright 2026 Abinash (https://github.com/AbiXnash)
