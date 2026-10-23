# Step 8 addition: sandbox identities cannot create ExternalSecret objects

Run 2026-10-21T14:25:10Z. `kubectl auth can-i --as` is not usable on this cluster: impersonation is dropped on the exec-plugin auth path (a made-up user "can" delete nodes), so every answer is the admin's. Checked instead with SubjectAccessReview, which the API server evaluates for the given user and groups.

```
control: made-up SA, delete nodes -> false
system:serviceaccount:openshell-openshell-spike-a:default create clusterexternalsecrets -n external-secrets -> false
system:serviceaccount:openshell-openshell-spike-a:default create clusterexternalsecrets -n koinon-spike -> false
system:serviceaccount:openshell-openshell-spike-a:default create clusterexternalsecrets -n openshell -> false
system:serviceaccount:openshell-openshell-spike-a:default create clusterexternalsecrets -n openshell-openshell-spike-a -> false
system:serviceaccount:openshell-openshell-spike-a:default create externalsecrets -n external-secrets -> false
system:serviceaccount:openshell-openshell-spike-a:default create externalsecrets -n koinon-spike -> false
system:serviceaccount:openshell-openshell-spike-a:default create externalsecrets -n openshell -> false
system:serviceaccount:openshell-openshell-spike-a:default create externalsecrets -n openshell-openshell-spike-a -> false
system:serviceaccount:openshell-openshell-spike-a:default create pods -n external-secrets -> false
system:serviceaccount:openshell-openshell-spike-a:default create pods -n koinon-spike -> false
system:serviceaccount:openshell-openshell-spike-a:default create pods -n openshell -> false
system:serviceaccount:openshell-openshell-spike-a:default create pods -n openshell-openshell-spike-a -> false
system:serviceaccount:openshell-openshell-spike-a:default create sandboxes -n external-secrets -> false
system:serviceaccount:openshell-openshell-spike-a:default create sandboxes -n koinon-spike -> false
system:serviceaccount:openshell-openshell-spike-a:default create sandboxes -n openshell -> false
system:serviceaccount:openshell-openshell-spike-a:default create sandboxes -n openshell-openshell-spike-a -> false
system:serviceaccount:openshell-openshell-spike-a:default create serviceaccounts/token -n external-secrets -> false
system:serviceaccount:openshell-openshell-spike-a:default create serviceaccounts/token -n koinon-spike -> false
system:serviceaccount:openshell-openshell-spike-a:default create serviceaccounts/token -n openshell -> false
system:serviceaccount:openshell-openshell-spike-a:default create serviceaccounts/token -n openshell-openshell-spike-a -> false
system:serviceaccount:openshell-openshell-spike-a:default get secrets -n external-secrets -> false
system:serviceaccount:openshell-openshell-spike-a:default get secrets -n koinon-spike -> false
system:serviceaccount:openshell-openshell-spike-a:default get secrets -n openshell -> false
system:serviceaccount:openshell-openshell-spike-a:default get secrets -n openshell-openshell-spike-a -> false
system:serviceaccount:openshell-openshell-spike-a:openshell-sandbox create clusterexternalsecrets -n external-secrets -> false
system:serviceaccount:openshell-openshell-spike-a:openshell-sandbox create clusterexternalsecrets -n koinon-spike -> false
system:serviceaccount:openshell-openshell-spike-a:openshell-sandbox create clusterexternalsecrets -n openshell -> false
system:serviceaccount:openshell-openshell-spike-a:openshell-sandbox create clusterexternalsecrets -n openshell-openshell-spike-a -> false
system:serviceaccount:openshell-openshell-spike-a:openshell-sandbox create externalsecrets -n external-secrets -> false
system:serviceaccount:openshell-openshell-spike-a:openshell-sandbox create externalsecrets -n koinon-spike -> false
system:serviceaccount:openshell-openshell-spike-a:openshell-sandbox create externalsecrets -n openshell -> false
system:serviceaccount:openshell-openshell-spike-a:openshell-sandbox create externalsecrets -n openshell-openshell-spike-a -> false
system:serviceaccount:openshell-openshell-spike-a:openshell-sandbox create pods -n external-secrets -> false
system:serviceaccount:openshell-openshell-spike-a:openshell-sandbox create pods -n koinon-spike -> false
system:serviceaccount:openshell-openshell-spike-a:openshell-sandbox create pods -n openshell -> false
system:serviceaccount:openshell-openshell-spike-a:openshell-sandbox create pods -n openshell-openshell-spike-a -> false
system:serviceaccount:openshell-openshell-spike-a:openshell-sandbox create sandboxes -n external-secrets -> false
system:serviceaccount:openshell-openshell-spike-a:openshell-sandbox create sandboxes -n koinon-spike -> false
system:serviceaccount:openshell-openshell-spike-a:openshell-sandbox create sandboxes -n openshell -> false
system:serviceaccount:openshell-openshell-spike-a:openshell-sandbox create sandboxes -n openshell-openshell-spike-a -> false
system:serviceaccount:openshell-openshell-spike-a:openshell-sandbox create serviceaccounts/token -n external-secrets -> false
system:serviceaccount:openshell-openshell-spike-a:openshell-sandbox create serviceaccounts/token -n koinon-spike -> false
system:serviceaccount:openshell-openshell-spike-a:openshell-sandbox create serviceaccounts/token -n openshell -> false
system:serviceaccount:openshell-openshell-spike-a:openshell-sandbox create serviceaccounts/token -n openshell-openshell-spike-a -> false
system:serviceaccount:openshell-openshell-spike-a:openshell-sandbox get secrets -n external-secrets -> false
system:serviceaccount:openshell-openshell-spike-a:openshell-sandbox get secrets -n koinon-spike -> false
system:serviceaccount:openshell-openshell-spike-a:openshell-sandbox get secrets -n openshell -> false
system:serviceaccount:openshell-openshell-spike-a:openshell-sandbox get secrets -n openshell-openshell-spike-a -> false
system:serviceaccount:openshell:openshell-sandbox create clusterexternalsecrets -n external-secrets -> false
system:serviceaccount:openshell:openshell-sandbox create clusterexternalsecrets -n koinon-spike -> false
system:serviceaccount:openshell:openshell-sandbox create clusterexternalsecrets -n openshell -> false
system:serviceaccount:openshell:openshell-sandbox create externalsecrets -n external-secrets -> false
system:serviceaccount:openshell:openshell-sandbox create externalsecrets -n koinon-spike -> false
system:serviceaccount:openshell:openshell-sandbox create externalsecrets -n openshell -> false
system:serviceaccount:openshell:openshell-sandbox create pods -n external-secrets -> false
system:serviceaccount:openshell:openshell-sandbox create pods -n koinon-spike -> false
system:serviceaccount:openshell:openshell-sandbox create pods -n openshell -> false
system:serviceaccount:openshell:openshell-sandbox create sandboxes -n external-secrets -> false
system:serviceaccount:openshell:openshell-sandbox create sandboxes -n koinon-spike -> false
system:serviceaccount:openshell:openshell-sandbox create sandboxes -n openshell -> false
system:serviceaccount:openshell:openshell-sandbox create serviceaccounts/token -n external-secrets -> false
system:serviceaccount:openshell:openshell-sandbox create serviceaccounts/token -n koinon-spike -> false
system:serviceaccount:openshell:openshell-sandbox create serviceaccounts/token -n openshell -> false
system:serviceaccount:openshell:openshell-sandbox get secrets -n external-secrets -> false
system:serviceaccount:openshell:openshell-sandbox get secrets -n koinon-spike -> false
system:serviceaccount:openshell:openshell-sandbox get secrets -n openshell -> false
```

Gateway service account (not a sandbox identity; for the record):
```
openshell:openshell create externalsecrets -n openshell-openshell-spike-a -> false
openshell:openshell create externalsecrets -n openshell -> false
openshell:openshell create clusterexternalsecrets -n <cluster> -> false
openshell:openshell get secrets -n openshell-openshell-spike-a -> false
openshell:openshell create secrets -n openshell-openshell-spike-a -> true
openshell:openshell get secrets -n koinon-spike -> false
```
