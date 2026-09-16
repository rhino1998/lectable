package audioworker

import (
	"fmt"
	"strconv"

	"github.com/rhino1998/lectable/audiocpp-go/audiocpp"
)

// Design renders a fresh reference clip via VoiceDesign ("vdes"), on a
// pooled, warm session for the resolved engine (see resolveDesignEngine -
// designModel selects which one, "" deferring to the process-wide
// default) - loadedDesign/Worker.getDesignModel's own doc comments cover
// the model+session pooling this now reuses across calls, mirroring
// Generate's own clone-session pool. Used to load a fresh Model/Session
// for every single call and close them again immediately after (see git
// history for that shape) - correctness-motivated, not just efficiency:
// concurrent Design calls each racing registry.LoadModel this way is what
// actually crashed a live ttsworker process during this app's own
// development (see jobs.maxDesignInFlight's own doc comment for that
// incident) - loadMu (worker.go) now serializes the LoadModel call itself
// process-wide regardless of this pool, and reusing an already-loaded
// model/session means that call only actually happens once per engine
// switch, not once per Design call at all.
func (w *Worker) Design(refText, instruct, language, designModel string, seed *int64) (*audiocpp.AudioBuffer, error) {
	w.touch()

	engine := resolveDesignEngine(designModel)
	ld, err := w.getDesignModel(engine)
	if err != nil {
		return nil, err
	}
	defer w.releaseDesignModel(ld)

	session, err := w.checkoutDesignSession(ld)
	if err != nil {
		return nil, err
	}

	instructOption := engine.instructOption
	if instructOption == "" {
		instructOption = "instruct"
	}

	lang := languageOption(language)
	if engine.noLanguageOption {
		lang = ""
	}

	request := audiocpp.NewRequest()
	defer request.Close()
	request.SetText(refText, lang).SetOption(instructOption, instruct)
	for k, v := range engine.defaultRequestOptions {
		request.SetOption(k, v)
	}
	if seed != nil {
		request.SetOption("seed", strconv.FormatInt(*seed, 10))
	}
	if err := request.Err(); err != nil {
		ld.avail <- session
		return nil, fmt.Errorf("build request: %w", err)
	}

	result, err := session.Run(request)
	if err != nil {
		// Returned to the pool rather than evicted, same as Generate's own
		// post-overflow handling (generate.go's own doc comment covers the
		// residual, not-fully-ruled-out risk that reasoning carries) - a
		// VoiceDesign call has none of Higgs's own warm-KV-cache-reuse
		// state to leave inconsistent after a failed run in the first
		// place, so there's less reason to think this needs the same
		// caution at all, just noting the parallel.
		ld.avail <- session
		return nil, fmt.Errorf("run: %w", err)
	}
	ld.avail <- session
	defer result.Close()

	audio, err := result.Audio()
	if err != nil {
		return nil, fmt.Errorf("read audio: %w", err)
	}
	if audio == nil {
		return nil, fmt.Errorf("design produced no audio")
	}
	return audio, nil
}
