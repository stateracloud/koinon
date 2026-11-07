# Platform agentgateway: Koinon's config and the OpenRouter model

Run 2026-10-21T18:09:38Z. Namespace `agentgateway` (the platform repository e4e93b6): Koinon owns ConfigMap `agentgateway-config`; the OpenRouter key is mounted from ExternalSecret `openrouter-api-key`.

```
$ kubectl -n agentgateway create configmap agentgateway-config --from-file=config.yaml=hack/spike/agentgateway/platform-config.yaml --dry-run=client -o yaml | kubectl apply -f -   # 18:09:38Z
configmap/agentgateway-config configured
```

```
$ [in sandbox or1] curl -H 'Authorization: Bearer $OPENAI_API_KEY' …agentgateway.agentgateway…:8080/v1/chat/completions model=qwen3.5:4b   # 18:11:07Z
model qwen3.5:4b provider None reply '' usage 19 200 error None
$ [in sandbox or1] curl -H 'Authorization: Bearer $OPENAI_API_KEY' …agentgateway.agentgateway…:8080/v1/chat/completions model=deepseek/deepseek-v4.1-flash   # 18:11:17Z
model deepseek/deepseek-v4.1-flash provider Baidu reply 'hello from koinon' usage 39 33 error None
$ kubectl -n agentgateway logs deploy/agentgateway (chat completions)
```

After adding the per-request routing guard (`overrides.provider`: zdr, data_collection deny, only the five allowlisted hosts, no fallbacks):
```
$ [in sandbox or1] chat completion model=deepseek/deepseek-v4.1-flash (run 1)   # 18:13:51Z
provider DeepInfra reply 'hello from koinon' error None
$ [in sandbox or1] chat completion model=deepseek/deepseek-v4.1-flash (run 2)   # 18:13:52Z
provider DeepInfra reply 'hello from koinon' error None
$ [in sandbox or1] chat completion model=deepseek/deepseek-v4.1-flash (run 3)   # 18:13:53Z
provider DeepInfra reply 'hello from koinon' error None
$ kubectl -n agentgateway logs deploy/agentgateway | koinon fields
200 None None qwen3.5:4b cost 0.000419
200 None None deepseek/deepseek-v4.1-flash cost 0.0000513
200 agent-a spike-a/agent-a deepseek/deepseek-v4.1-flash cost 0.0000405
200 agent-a spike-a/agent-a deepseek/deepseek-v4.1-flash cost 0.0000513
200 agent-a spike-a/agent-a deepseek/deepseek-v4.1-flash cost 0.0000429
```
