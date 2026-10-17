# Per-host model sources

`agentmux models list` reads `~/.config/agentmux/models.yaml` and checks each
configured endpoint. Use `-config PATH` to inspect another registry. The file
is host-local and should not be committed to a project repository.

```yaml
sources:
  - name: local
    kind: ollama-local       # ollama-cloud, ollama-local, openai-compatible, subscription
    endpoint: http://127.0.0.1:11434
    cost: free               # free, subscription, metered
    limits:
      - local capacity
    expires: 2027-01-31       # optional, YYYY-MM-DD
    telemetry: false
    training: false
  - name: hosted
    kind: openai-compatible
    endpoint: https://models.example.invalid/v1
    credential_ref: op://vault/item/credential
    cost: metered
    telemetry: true
    training: false
```

`credential_ref` must be an `op://` reference. The CLI resolves it through
`op run` and passes it to the one-shot reachability probe as an environment
variable; the secret is never written into the registry or probe output.
Omit credentials for endpoints that do not require authentication. A
subscription source can omit its endpoint, in which case it is reported as
unavailable because there is no endpoint to check.

The list reports expired and unreachable entries with a reason. JSON output
omits credential references.
