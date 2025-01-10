# Make release

```shell
git pull release-please--branches--main
git tag v0.0.2

make helm-unit docs

git add .
git commit -s --amend
```
