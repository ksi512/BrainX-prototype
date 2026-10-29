# 发布与签名

## CI

- `.github/workflows/ci.yml`：Ubuntu / macOS / Windows 三平台跑 `gofmt`、`go vet`、`go test`、`go build`；Ubuntu 另跑 `go test -race`。
- `.github/workflows/release.yml`：推送 `v*` tag 后构建 6 个平台的包并上传 GitHub Release。
- Release workflow 会拒绝仍含 `<仓库地址>` 占位符的 README。

## 本地构建

```bash
VERSION=v1.0.0 ./scripts/release.sh
```

产物在 `dist/`，并生成 `checksums.txt`。

## macOS 签名 + notarization

公开分发 macOS 二进制时，建议做 Developer ID 签名和 notarization；否则 Gatekeeper 会拦截。

1. 准备 **Developer ID Application** 证书，导出 `.p12`。
2. 对每个二进制签名（arm64 / amd64 各一次）：

   ```bash
   codesign --force --options runtime --timestamp \
     --sign "Developer ID Application: <Your Name> (<TEAMID>)" \
     brainx
   codesign --verify --deep --strict --verbose=2 brainx
   ```

3. 打包后提交 Apple notarization：

   ```bash
   xcrun notarytool submit brainx_v1.0.0_darwin_arm64.zip \
     --apple-id "$APPLE_ID" \
     --team-id "$APPLE_TEAM_ID" \
     --password "$APPLE_APP_PASSWORD" \
     --wait
   xcrun stapler staple brainx_v1.0.0_darwin_arm64.zip
   ```

4. 验证：

   ```bash
   spctl -a -vv -t install brainx
   ```

CI 中需要把这些值放进 GitHub Secrets：

- `MACOS_CERT_P12`（base64）
- `MACOS_CERT_PASSWORD`
- `APPLE_ID`
- `APPLE_TEAM_ID`
- `APPLE_APP_PASSWORD`

在 `release.yml` 里导入证书后，对 `dist/*darwin*` 执行签名和 notarization；不在 CI 里做也可以，发布前在本机手动签。

## Windows 签名

公开分发 Windows 二进制时，用 Authenticode 证书签 `brainx.exe`：

```powershell
signtool sign /fd SHA256 /tr http://timestamp.digicert.com /td SHA256 `
  /f cert.pfx /p "$env:WINDOWS_CERT_PASSWORD" brainx.exe
signtool verify /pa brainx.exe
```

CI 里把 `.pfx` 和密码放进 Secrets；没有证书也可以先发布，但 SmartScreen 会提示未知发布者。

## Linux

Linux 没有 notarization；建议：

```bash
gpg --detach-sign --armor checksums.txt
# 或
minisign -Sm checksums.txt
```

把公钥和签名一起上传 Release。

## 建议顺序

1. 先让 CI 三平台全绿。
2. 打 `v1.0.0` tag，自动生成包。
3. 有 Apple/Windows 证书后，再加签名步骤。
4. 最后把 `checksums.txt` 和签名一起发布。
