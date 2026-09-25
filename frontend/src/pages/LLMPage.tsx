import { useState } from 'react'
import { useTestLLM } from '../api/queries'
import { ApiError } from '../api/client'

// Standalone raw-prompt test page for this app's own embedded speaker-
// attribution GGUF model (see backend/CLAUDE.md's "Speaker attribution"
// section) - a system+user prompt pair in, plain generated text out, no
// speakerattr-specific framing (attribution/characterization/direction
// JSON shapes etc.). Stateless, same shape as SFXPage/VoicesPage's own
// "test" flows - nothing here is saved.
export function LLMPage() {
  const testLLM = useTestLLM()

  const [systemPrompt, setSystemPrompt] = useState('')
  const [userPrompt, setUserPrompt] = useState('')
  // Text, not number - an empty string is exactly "unset, defer to the
  // backend's own default" (see TestLLMRequest), which a bare `0` can't
  // distinguish from "the reader typed zero".
  const [temp, setTemp] = useState('')
  const [maxTokens, setMaxTokens] = useState('')

  const [result, setResult] = useState<string | null>(null)
  const [error, setError] = useState<string | null>(null)

  const run = () => {
    if (!userPrompt.trim()) return
    setError(null)
    setResult(null)
    testLLM.mutate(
      {
        systemPrompt: systemPrompt.trim() || undefined,
        userPrompt: userPrompt.trim(),
        temp: temp ? Number(temp) : undefined,
        maxTokens: maxTokens ? Number(maxTokens) : undefined,
      },
      {
        onSuccess: (res) => setResult(res.text),
        onError: (err) => setError(err instanceof ApiError ? err.message : 'Could not generate text'),
      },
    )
  }

  return (
    <div className="voices-page">
      <div className="library-header">
        <h1>LLM</h1>
      </div>
      <p className="muted">
        Test a raw system/user prompt pair against this app's own embedded speaker-attribution model. Nothing here
        is saved.
      </p>
      <div className="custom-voice-form">
        <label>
          System prompt (optional)
          <textarea
            value={systemPrompt}
            onChange={(e) => setSystemPrompt(e.target.value)}
            rows={3}
            placeholder="You are a literary analysis assistant..."
          />
        </label>
        <label>
          User prompt
          <textarea
            value={userPrompt}
            onChange={(e) => setUserPrompt(e.target.value)}
            rows={4}
            placeholder="Type the user turn to send..."
          />
        </label>
        <label>
          Temperature
          <input type="number" min={0} max={2} step={0.05} value={temp} onChange={(e) => setTemp(e.target.value)} />
        </label>
        <label>
          Max tokens
          <input
            type="number"
            min={1}
            step={1}
            value={maxTokens}
            onChange={(e) => setMaxTokens(e.target.value)}
          />
        </label>
        {error && <p className="error-text">{error}</p>}
        <div className="voice-panel-actions">
          <button className="primary-button" onClick={run} disabled={!userPrompt.trim() || testLLM.isPending}>
            {testLLM.isPending ? 'Generating…' : 'Generate'}
          </button>
        </div>
        {result !== null && <pre className="paragraph-generation-text">{result}</pre>}
      </div>
    </div>
  )
}
