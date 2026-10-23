# --no-keep orphan: evidence

Run 2026-10-21T16:09:33Z with gateway `log_level = debug` (Helm revision 4). Each run: `openshell sandbox create --name nkN --no-keep -- true`, then the gateway's list and the cluster's Sandbox/PVC.

```
$ openshell sandbox create --name nk1 --no-keep -- true   # 16:09:35Z

gateway lists nk1: 0
cluster: Error from server | Error from
$ openshell sandbox create --name nk2 --no-keep -- true   # 16:09:44Z
✓ Deleted sandbox nk2
gateway lists nk2: 0
cluster: Error from server | Error from
$ openshell sandbox create --name nk3 --no-keep -- true   # 16:10:22Z
✓ Deleted sandbox nk3
gateway lists nk3: 0
cluster: Error from server | Error from
$ openshell sandbox create --name nk4 --no-keep -- true   # 16:11:00Z
✓ Deleted sandbox nk4
gateway lists nk4: 0
cluster: Error from server | Error from
$ openshell sandbox create --name nk5 --no-keep -- true   # 16:11:38Z
✓ Deleted sandbox nk5
gateway lists nk5: 0
cluster: Error from server | Error from
```

Gateway debug log, last 10 min: 59 lines matching 'already deleted|replaced|409|conflict|precondition'. Sandbox watch: 42 events; every nkN ended with DELETED.

Not reproduced in 5 runs (nk1 printed nothing: the CLI's port-forward was reconnecting after the gateway restart). The two earlier orphans (`p3`, `a16b`) remain the only occurrences; the suspected cause from code reading (409 treated like 404 in `delete_sandbox_inner`) is unconfirmed. Not filing upstream without a reproduction.
