# Index protocol schemas and fixtures

Copied from [gotalk-platform](https://github.com/parkerbrown98/gotalk-platform)'s
`packages/index-protocol` (`schemas/` and `fixtures/`). The index tests validate every response from
`/api/v1/index/*` and `/.well-known/gotalk-instance` against these schemas, and check that every fixture
validates, so a change on either side shows up here. The ping signing vector lives in
`internal/indexsig/testdata`.

To update, copy the directories again from the platform repo:

```sh
P=../gotalk-platform/packages/index-protocol
cp $P/schemas/*.json internal/api/testdata/index-protocol/schemas/
cp $P/fixtures/*.json internal/api/testdata/index-protocol/fixtures/
mv internal/api/testdata/index-protocol/fixtures/signing-vector.json internal/indexsig/testdata/
```
