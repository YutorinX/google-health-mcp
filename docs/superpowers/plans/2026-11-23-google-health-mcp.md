# 自分用 Google Health MCP サーバー 実装計画

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** フォークに小さな修正を加え、Cloud Run に置いて claude.ai（Web とスマホ）から自分の健康データを参照できるようにする。

**Architecture:** 上流 1874a2c の hosted モードをそのまま使う。コードの変更はデプロイスクリプト、ログ出力1か所の整形、Dockerfile の固定、`mise.toml` だけ。GCP の準備と接続確認は利用者と一緒に行う。

**Tech Stack:** Go 1.27.4（mise）、modelcontextprotocol/go-sdk v1.8.0、golang.org/x/oauth2 v0.38.0、Cloud Run、Secret Manager、gcloud

**Spec:** `docs/superpowers/specs/2026-11-22-google-health-mcp-design.md`

## Global Constraints

- 健康データの測定値・日時・端末の個体情報をリポジトリに書かない（公開フォーク）
- メールアドレス、プロジェクト ID、OAuth クライアントの値をコミットしない
- 依存パッケージを増やさない
- 外部ライブラリや配線を差し替えるテストは足さない。足すのは自前の純粋なロジックのテストだけ
- コメントは、無いとコードが読めないときだけ書く
- 上流への PR・Issue は作らない
- Bash の呼び出しにシェルコメントを入れない。`git push`・`gh` は単独で叩く
- `go test` はローカルのポートを開くため、サンドボックスの外で実行する（利用者の許可済み）
- PR は `gh pr create --repo YutorinX/google-health-mcp --base main -a @me --body-file <ファイル>` で作る。`--repo` を省くと上流宛てになる

## Review Focus

- `ALLOWED_REDIRECT_HOSTS` を空文字で渡した → 制限が外れず既定値 `claude.ai,claude.com` になる（Task 1 の確認手順で検証）
- `cloudrun.sh deploy` を2回目以降に実行した → 戻り先の制限と max instances が保たれる（Task 1 の確認手順で検証）
- `x/oauth2` のエラーが `fmt.Errorf("...: %w", err)` で包まれて届いた → 包まれていない場合と同じ要約が出る（Task 2 のテスト）
- Google が `error` コードの無い HTML を返した → ログに本文が出ない（Task 2 のテスト）
- Cloud Build（linux/amd64）でのビルド → ダイジェスト固定後もベースイメージが解決できる。マルチアーキテクチャの index のダイジェストを使う（Task 3 の確認手順で検証）

---

## コード変更（Task 1〜3）

作業ブランチは `harden-for-personal-use`（設計書のコミット済み）。Task 1〜3 は subagent に任せてよい。

### Task 1: デプロイスクリプトと mise

**Files:**
- Modify: `deploy/cloudrun.sh:17-26`（コメントと変数）、`deploy/cloudrun.sh:101,103`
- Create: `mise.toml`
- Modify: `.gitignore`

**Interfaces:**
- Produces: `cloudrun.sh` が読む環境変数 `ALLOWED_REDIRECT_HOSTS`（既定 `claude.ai,claude.com`）と `MAX_INSTANCES`（既定 `1`）。Task 6 が使う

- [ ] **Step 1: `mise.toml` を作る**

`[tools]` に `go = "1.27.4"` と shellcheck を置く。shellcheck は `mise use shellcheck@latest` で入れ、`mise.toml` には入った具体的なバージョンを書く（`latest` のまま残さない）。

- [ ] **Step 2: `.gitignore` に `/bin/` と `TODO.local.md` を足す**

- [ ] **Step 3: `cloudrun.sh` を直す**

- 変数定義に `REDIRECT_HOSTS="${ALLOWED_REDIRECT_HOSTS:-claude.ai,claude.com}"` と `MAX_INSTANCES="${MAX_INSTANCES:-1}"` を足す。`:-` にして空文字でも既定値になるようにする
- 101行目を `--min-instances 0 --max-instances "$MAX_INSTANCES"` にする
- 103行目の `--set-env-vars` に `|ALLOWED_REDIRECT_HOSTS=$REDIRECT_HOSTS` を足す
- 17-19行目の Optional の説明に2つの変数を足す

- [ ] **Step 4: 静的検査**

Run: `mise exec -- shellcheck deploy/cloudrun.sh deploy/setup-github-ci.sh`
Expected: 出力なし、終了コード 0

- [ ] **Step 5: 偽の gcloud で渡す引数を確かめる**

scratchpad に、受け取った引数を1行で標準出力へ出し、`projects describe` のときだけ `123456` を返す `gcloud` スクリプトを置く。リポジトリには入れない。`PATH` の先頭にその場所を足し、次の2通りで `PROJECT_ID=p ALLOWED_EMAILS=a@example.com GOOGLE_OAUTH_CLIENT_ID=c GOOGLE_OAUTH_CLIENT_SECRET=s ./deploy/cloudrun.sh deploy` を実行する。

1. `ALLOWED_REDIRECT_HOSTS` と `MAX_INSTANCES` を設定しない
2. `ALLOWED_REDIRECT_HOSTS=` （空文字）を設定する

Expected: どちらも `run deploy` の行に `--max-instances 1` と `ALLOWED_REDIRECT_HOSTS=claude.ai,claude.com` が含まれる。最後の `curl` の失敗は警告で終わる。

- [ ] **Step 6: Commit**

```bash
git add mise.toml .gitignore deploy/cloudrun.sh
git commit -m "deploy: 戻り先ホストの制限と max instances をスクリプトで渡す"
```

### Task 2: Google のエラーをログに出す前に要約する

**Files:**
- Create: `internal/remote/googleerr.go`
- Create: `internal/remote/googleerr_test.go`（`package remote`）
- Modify: `internal/remote/remote.go:484,586,626`

**Interfaces:**
- Produces: `func googleErrSummary(err error) string`

- [ ] **Step 1: 失敗するテストを書く**

`TestGoogleErrSummary` をテーブル形式で書く。入力は `*oauth2.RetrieveError` を直接組み立てる（`Response: &http.Response{StatusCode: ...}`, `Body`, `ErrorCode`）。

| ケース | 入力 | 含む | 含まない |
|---|---|---|---|
| コードあり | 400, `invalid_grant`, 本文 `SECRET` | `400`, `invalid_grant` | `SECRET` |
| コードなし | 502, コード空, 本文 `<html>SECRET` | `502` | `SECRET`, `<html>` |
| 包まれている | 上の1行目を `fmt.Errorf("x: %w", e)` | `400`, `invalid_grant` | `SECRET` |
| Response が nil | コード `invalid_grant` | `invalid_grant` | — |
| 別の型 | `errors.New("dial SECRET")` | — | `SECRET` |

- [ ] **Step 2: 失敗を確かめる**

Run: `mise exec -- go test ./internal/remote/ -run TestGoogleErrSummary`
Expected: FAIL（`googleErrSummary` が未定義）

- [ ] **Step 3: `googleErrSummary` を実装する**

`errors.As` で `*oauth2.RetrieveError` を取り出し、HTTP ステータスと `ErrorCode` だけを返す。取り出せなければ `fmt.Sprintf("%T", err)` を返す。文言は任せる。

- [ ] **Step 4: 3か所を置き換える**

`remote.go:484,586,626` の `%v", ..., err` を `%s", ..., googleErrSummary(err)` にする。ログの固定文言と `userRef` はそのまま。

- [ ] **Step 5: テスト一式を通す**

Run: `mise exec -- go vet ./...` と `mise exec -- go test -race -count=1 ./...`（サンドボックス外）
Expected: vet は出力なし、test はすべて `ok`

- [ ] **Step 6: Commit**

```bash
git add internal/remote/googleerr.go internal/remote/googleerr_test.go internal/remote/remote.go
git commit -m "remote: Google のエラー本文をログに出さない"
```

### Task 3: ベースイメージをダイジェストで固定する

**Files:**
- Modify: `Dockerfile:2,14`

- [ ] **Step 1: 2つのイメージの index ダイジェストを調べる**

`golang:1.27` と `gcr.io/distroless/static-debian12:nonroot` の、マルチアーキテクチャ index のダイジェストを取る（`docker buildx imagetools inspect <image>` の `Digest:` など。手段は任せる）。プラットフォーム別のダイジェストは使わない。

- [ ] **Step 2: `FROM` を `image:tag@sha256:<digest>` の形にする**

タグも残す。dependabot の docker 更新がタグとダイジェストの両方を更新できる形にするため。

- [ ] **Step 3: ビルドを確かめる**

Run: `docker build --platform linux/amd64 -t ghm-local .`
Expected: 成功。Docker のデーモンが動いていなければ、その旨を報告して Task 6 の Cloud Build で確かめる

- [ ] **Step 4: Commit**

```bash
git add Dockerfile
git commit -m "docker: ベースイメージをダイジェストで固定"
```

### Task 3.5: PR を作る

- [ ] `git push -u origin harden-for-personal-use` を単独で実行する
- [ ] PR 本文をファイルに書き、Global Constraints の `gh pr create` で PR を作る。本文は、この PR が何を変えるかだけを書く
- [ ] マージは利用者に確認してから行う

---

## 利用者と行う作業（Task 4〜8）

ブラウザでのログインやコンソールの操作が要るため、親のセッションで利用者と進める。subagent には任せない。

### Task 4: GCP の準備（ローカル確認に要る分）

- [ ] プロジェクト ID を利用者に決めてもらい、`gcloud projects create <ID>` で作る
- [ ] 請求先アカウントを利用者に確認し、`gcloud billing projects link <ID> --billing-account <ACCOUNT>` で紐付ける
- [ ] `gcloud services enable health.googleapis.com --project <ID>`
- [ ] 予算アラートを作る: `gcloud billing budgets create --billing-account <ACCOUNT> --display-name google-health-mcp --budget-amount <5ドル相当><通貨> --threshold-rule percent=0.5 --threshold-rule percent=0.9 --threshold-rule percent=1.0 --filter-projects projects/<ID>`。通貨は請求先アカウントに合わせる
- [ ] 利用者がコンソールで Google Auth Platform を設定する: ブランディング、対象（外部・テスト・自分をテストユーザーに追加）、データアクセス（既定5スコープと `openid`, `email`）。スコープの一覧は `internal/auth/auth.go:36-42`
- [ ] 利用者が Desktop app の OAuth クライアントを作り、JSON を `~/Library/Application Support/google-health-mcp/credentials.json` に置く

完了条件: `gcloud services list --enabled --project <ID>` に `health.googleapis.com` がある。予算が `gcloud billing budgets list` に出る。

### Task 5: ローカルでの確認とデータ型の表

- [ ] `mise exec -- go build -o bin/google-health-mcp .`
- [ ] `bin/google-health-mcp login` を利用者に実行してもらう（ブラウザでログイン）。`bin/google-health-mcp status` で接続を確かめる
- [ ] `claude mcp add google-health-local --env GOOGLE_HEALTH_TZ=Asia/Tokyo -- <絶対パス>/bin/google-health-mcp serve`
- [ ] 利用者に、Health Connect にデータを書いている Pixel Watch 以外のアプリを挙げてもらう
- [ ] 新しい Claude Code のセッションで `list_data_types` を呼び、各データ型について直近30日を `get_data_points`（`verbose=true`）で取得する。`dataSource` の platform と device の種類を記録する。運動の記録に GPS ルートが含まれるかも見る（設計書の未確認事項 5）
- [ ] `docs/data-availability.md` に表を書く: データ型 / 返るか / 記録元の種類。測定値・日時・端末の個体情報は書かない
- [ ] 表を利用者に見せてから commit する

### Task 6: 公開ステータスの確認とデプロイ

- [ ] 利用者がコンソールで公開ステータスを「本番環境」にする。`bin/google-health-mcp logout` と `login` をやり直し、「未確認のアプリ」の警告を進んでログインできるかを見る。できなければ「テスト」に戻し、週1回の再接続で運用する（設計書の未確認事項 1）
- [ ] `PROJECT_ID=<ID> REGION=asia-northeast1 ./deploy/cloudrun.sh url` で戻り先 URI を出す
- [ ] 利用者が Web application の OAuth クライアントを作り、その URI を承認済みのリダイレクト URI に登録する
- [ ] `PROJECT_ID=<ID> REGION=asia-northeast1 ALLOWED_EMAILS=<利用者のアドレス> GOOGLE_HEALTH_TZ=Asia/Tokyo GOOGLE_OAUTH_CLIENT_ID=<ID> GOOGLE_OAUTH_CLIENT_SECRET=<secret> ./deploy/cloudrun.sh deploy`。秘密の値はコマンド履歴に残らない渡し方を利用者と決める
- [ ] `gcloud run services describe google-health-mcp --region asia-northeast1 --project <ID> --format yaml` で確かめる: `GOOGLE_HEALTH_API_BASE` が無い、`ALLOWED_REDIRECT_HOSTS=claude.ai,claude.com` がある、`autoscaling.knative.dev/maxScale: '1'`
- [ ] 戻り先の制限を確かめる: `POST <URL>/oauth/register` に `{"redirect_uris":["https://evil.example/cb"]}` → 400。`{"redirect_uris":["https://claude.ai/api/mcp/auth_callback"],"client_name":"check"}` → 201
- [ ] `TODO.local.md` に、本番環境にできた場合は8日後に再接続なしで使えるかを確かめる、と日付つきで書く（設計書の未確認事項 2）

### Task 7: claude.ai への接続

- [ ] 利用者が claude.ai の Web で、設定 → コネクタ → カスタムコネクタを追加し、`<URL>/mcp` を登録してログインする。登録に失敗したら、claude.ai の戻り先 URL を Anthropic のサポート記事で確かめる（設計書の未確認事項 3）
- [ ] 「昨日の睡眠と歩数を教えて」などで、ツールが答えを返すことを確かめる
- [ ] スマホアプリで同じコネクタが使えることを確かめる
- [ ] 許可外の Google アカウントでログインすると 403 のページが出ることを確かめる

### Task 8: ツールとスコープの提案

- [ ] `docs/tools-and-scopes-proposal.md` に、Task 5 の表をもとに提案を書く: 外せるツール、外せるスコープ、既定スコープを減らすためのコード変更の場所（`internal/auth/auth.go:36-42`）と変更量。実装はしない
- [ ] 利用者に見せてから commit し、PR にする
