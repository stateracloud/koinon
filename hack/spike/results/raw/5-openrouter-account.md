# OpenRouter account settings, checked without Koinon's routing guard

Run 2026-10-21T18:24:13Z from the Mac, straight to openrouter.ai with the koinon-test key, no `provider` field. procoduck applied: ZDR for every model group, training off, allowed providers fireworks/together/deepinfra/amazon-bedrock/google-vertex, prompt logging off. (The request below shows the account list actually holds 16 providers; procoduck confirmed the wider list is intentional.)

```
deepseek/deepseek-v4.1-flash -> provider DeepInfra error None
deepseek/deepseek-v4.1-flash -> provider DeepInfra error None
z-ai/glm-5.3-flash -> provider Fireworks error None
unguarded request asking for baidu -> provider None error No allowed providers are available for the selected model. Providers serving deepseek/deepseek-v4.1-flash-20260910: open-inference, baidu, relace, decart, sail-research, inference-net, reka, atlas-cloud, deepinfra, streamlake, coreweave, alibaba, gmicloud, wafer, novita, phala, morph, ionstream, digitalocean, makora, crusoe, google-vertex, nebius, primeintellect, baseten, together, siliconflow, modal, parasail, fireworks, venice, but your account's allowed-providers setting permits only: xai, groq, azure, google-vertex, nvidia, openai, mistral, deepseek, typesafe, anthropic, deepinfra, fireworks, cloudflare, moonshotai, amazon-bedrock, claude-on-aws. To change your allowed providers, visit: https://openrouter.ai/settings/privacy. Additionally, your request's provider.only preference permits only: baidu.
```
