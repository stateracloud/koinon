# Step 7 prerequisite: can the gateway exec stream carry ACP stdio?

Run 2026-10-21T14:40Z in sandbox `p2`.

```
$ openshell sandbox exec --help | grep -- --tty
      --tty
      --no-tty

$ printf '{"a":1}\n{"b":2}\n' | openshell sandbox exec --name p2 --no-tty -- sh -c 'while read l; do echo "got:$l"; done' | od -c
0000000    g   o   t   :   {   "   a   "   :   1   }  \n   g   o   t   :
0000020    {   "   b   "   :   2   }  \n

$ ( printf 'one\n'; sleep 2; printf 'two\n' ) | openshell sandbox exec --name p2 --no-tty -- sh -c 'while read l; do echo "$(date +%s) $l"; done'
1792593610 one
1792593612 two
```

With `--no-tty`, stdin is streamed incrementally and bytes pass unchanged (LF, no CR), so an ACP
client can drive a harness as `openshell sandbox exec --no-tty -- <harness> acp` without a relay
inside the sandbox. The Go SDK's `Exec().Stream` has no stdin and `Exec().Interactive` allocates a
TTY (echo, CRLF), so `hack/spike/acpdrive` spawns the CLI instead; Koinon's `agentd` will need the
non-TTY stdin path in the gRPC API (gap for 4.7).
