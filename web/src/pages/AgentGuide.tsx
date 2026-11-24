import { Link } from 'react-router-dom';
import { api } from '../api';
import { useLoad } from '../useLoad';
import { Banner, CodeBlock, CopyField, Loading } from '../components/ui';

/**
 * Integration guide for AI agents: the public markdown URL to hand to an
 * agent, token creation, MCP config and the REST fallback. Content mirrors
 * the embedded guide served at {relay}/v1/agent-guide — keep them in sync.
 */
export default function AgentGuidePage() {
  const info = useLoad(() => api.info(), []);
  // relay_url is the server root (PUBLIC_RELAY_URL convention: no /v1);
  // the guide, MCP and utilization endpoints link off it directly.
  const relay = (info.data?.relay_url ?? '').replace(/\/+$/, '');

  const guideUrl = relay ? `${relay}/v1/agent-guide` : '';
  const mcpConfig = relay
    ? JSON.stringify(
        {
          mcpServers: {
            'llm-relay': {
              type: 'http',
              url: `${relay}/mcp`,
              headers: { Authorization: 'Bearer sk-YOUR_KEY' },
            },
          },
        },
        null,
        2,
      )
    : '';
  const curl = relay
    ? `curl -s ${relay}/v1/utilization \\\n  -H "Authorization: Bearer sk-YOUR_KEY"`
    : '';
  const modelsCurl = relay
    ? `curl -s ${relay}/v1/models \\\n  -H "Authorization: Bearer sk-YOUR_KEY"`
    : '';
  const modelsJson = `{
  "object": "list",
  "data": [
    { "id": "halogen-flash", "object": "model", "owned_by": "llm-relay" }
  ]
}`;
  const chatCurl = relay
    ? `curl -s ${relay}/v1/chat/completions \\\n  -H "Authorization: Bearer sk-YOUR_KEY" \\\n  -H "Content-Type: application/json" \\\n  -d '{\n    "model": "halogen-flash",\n    "messages": [{ "role": "user", "content": "Hello!" }],\n    "max_tokens": 100\n  }'`
    : '';
  const chatJson = `{
  "id": "chatcmpl-...",
  "object": "chat.completion",
  "model": "halogen-flash",
  "choices": [
    {
      "index": 0,
      "message": { "role": "assistant", "content": "Hi!" },
      "finish_reason": "stop"
    }
  ],
  "usage": { "prompt_tokens": 9, "completion_tokens": 5, "total_tokens": 14 }
}`;

  return (
    <div className="page">
      <div className="page-head">
        <h2>AI agent integration</h2>
        <span className="dim">let agents check if this server is free — then use it</span>
      </div>

      {!info.loading && !info.data && (
        <Banner>Could not load server info: {info.error}</Banner>
      )}
      {info.loading && !info.data && <Loading />}

      <section className="panel">
        <h3>Give this link to your AI</h3>
        <p className="dim">
          Your agent can read this page without any token — it explains the
          whole integration, including how to get an access key. Paste the
          link into your agent and say “read this and set it up”.
        </p>
        <CopyField
          value={guideUrl}
          hint="Not configured by the admin (PUBLIC_RELAY_URL)."
        />
        <p className="dim">
          Or show your agent this page — it contains the same instructions.
        </p>
      </section>

      <section className="panel">
        <h3>1. Access token</h3>
        <p className="dim">
          Agents authenticate with a normal relay API key — the same{' '}
          <code className="mono">sk-…</code> keys used for inference. Create
          one named after the agent on the <Link to="/keys">My keys</Link>{' '}
          page; the full key is shown only once.
        </p>
      </section>

      <section className="panel">
        <h3>2. MCP (recommended)</h3>
        <p className="dim">
          One URL in the agent’s MCP config gives it a{' '}
          <code className="mono">check_utilization</code> tool: per model{' '}
          <code className="mono">online</code>,{' '}
          <code className="mono">active</code>,{' '}
          <code className="mono">concurrency_limit</code>,{' '}
          <code className="mono">utilization_pct</code> (mean busy % of the
          last 5 min) and a derived{' '}
          <code className="mono">status</code>.
        </p>
        {mcpConfig && <CodeBlock text={mcpConfig} />}
      </section>

      <section className="panel">
        <h3>3. REST fallback</h3>
        <p className="dim">
          Works with any HTTP client; same payload as the MCP tool.
        </p>
        {curl && <CodeBlock text={curl} />}
      </section>

      <section className="panel">
        <h3>4. Use it for inference</h3>
        <p className="dim">
          Once a model is <code className="mono">free</code>, the relay is a
          plain OpenAI-compatible endpoint: point any OpenAI client (or{' '}
          <code className="mono">curl</code>) at it with the same{' '}
          <code className="mono">sk-…</code> key. Base URL{' '}
          <code className="mono">{relay ? `${relay}/v1` : '{relay}/v1'}</code>,
          header <code className="mono">Authorization: Bearer sk-YOUR_KEY</code>.
        </p>
        <p className="dim">
          <strong>List models</strong> — each <code className="mono">id</code>{' '}
          is a name you can request; a model group shows as one name and the
          relay picks a member.
        </p>
        {modelsCurl && <CodeBlock text={modelsCurl} />}
        <CodeBlock text={modelsJson} />
        <p className="dim">
          <strong>Generate</strong> — <code className="mono">POST /v1/chat/completions</code>{' '}
          (and <code className="mono">POST /v1/completions</code>) take the
          usual OpenAI body; <code className="mono">model</code> is required
          (unknown name → <code className="mono">404</code>), and{' '}
          <code className="mono">"stream": true</code> switches to SSE. The
          body is the backend’s reply, passed through untouched.
        </p>
        {chatCurl && <CodeBlock text={chatCurl} />}
        <CodeBlock text={chatJson} />
      </section>

      <section className="panel">
        <h3>Status values</h3>
        <div className="table-scroll">
          <table>
            <thead>
              <tr>
                <th>status</th>
                <th>meaning</th>
              </tr>
            </thead>
            <tbody>
              <tr>
                <td>
                  <span className="badge ok">free</span>
                </td>
                <td>capacity available — delegate here.</td>
              </tr>
              <tr>
                <td>
                  <span className="badge warn">busy</span>
                </td>
                <td>
                  all slots in use or ≥ 80 % busy — wait and re-check, or
                  pick another model.
                </td>
              </tr>
              <tr>
                <td>
                  <span className="badge err">offline</span>
                </td>
                <td>backend unreachable — skip this server.</td>
              </tr>
            </tbody>
          </table>
        </div>
        <p className="dim">
          The check is advisory: there is no reservation, so re-check on 429
          or 503.
        </p>
      </section>
    </div>
  );
}
