# GM Suite — Ứng dụng Desktop Tự động hoá EVM

**Ngôn ngữ:** [English](README.md) · **Tiếng Việt** · [中文](README.zh.md)

Ứng dụng desktop độc lập cho tự động hoá EVM khối lượng lớn: mint NFT đa ví (SeaDrop + voucher presale của OpenSea), bundle riêng tư Flashbots, kiểm tra điều kiện whitelist, quản lý RPC/proxy và điều khiển từ xa qua Telegram — tất cả trong một cửa sổ native duy nhất.

Viết bằng Go (một file binary, SQLite thuần Go, không CGO) với giao diện WebView2. **Chỉ hỗ trợ EVM.**

> ⚠️ **Miễn trừ trách nhiệm.** Công cụ này xử lý private key và tự động gửi giao dịch on-chain. Chỉ dùng với ví và tài sản bạn sở hữu. Không có bảo hành — bạn tự chịu trách nhiệm với những gì ký và phát đi. Đây không phải lời khuyên tài chính.

---

## Tính năng

- **Ví (Wallets)** — Tạo hoặc import hàng loạt, phân nhóm. Private key được **mã hoá khi lưu** bằng một secret ngẫu nhiên lưu cục bộ (`vault.key`) và tự mở khoá khi khởi động; chức năng hiện key mặc định tắt.
- **Tác vụ (engine mint)**
  - **Mint public** SeaDrop và mint **presale có chữ ký/voucher** (allowlist) của OpenSea.
  - Bundle riêng tư **Flashbots** (chống front-run) với cửa sổ bundle + phí priority/max tuỳ chỉnh.
  - **Simulate → execute**: chạy thử từng giao dịch (`eth_call`), chỉ phát đi nếu chắc chắn thành công.
  - **Hành động nối tiếp sau mint**: tự động Transfer / List / Accept-offer / Drain sau khi mint thành công.
  - **Hẹn giờ chạy**, chọn nhiều RPC cho từng task, gán nhóm proxy.
- **Whitelist Checker** — Đăng nhập từng ví vào OpenSea (SIWE) và đọc **điều kiện theo từng phase** (WL / presale / public) kèm cap tích lũy. Kết quả **hiện dần từng dòng**; cấu hình **số luồng** + **nhóm proxy** ngay trong tab để tránh bị chặn theo IP.
- **NFT Manager** — Xem holdings, list/huỷ listing trên OpenSea (Seaport).
- **RPC** — Quản lý endpoint đa chain, đo độ trễ; fallback theo từng chain.
- **Proxies** — Nhóm, xoay vòng và test khả năng kết nối trực tiếp.
- **Công cụ Contract** — Dán/fetch ABI + chọn hàm cho contract bất kỳ; replay giao dịch theo tx hash hoặc link explorer.
- **Cài đặt (Settings)** — API key OpenSea (**nhiều key, tự xoay vòng** để né rate limit), key Etherscan, tinh chỉnh Flashbots, **chain tuỳ chỉnh** (thêm chain mới không cần sửa code), thông báo Discord webhook, tỷ lệ phóng to giao diện và kiểm tra cập nhật.
- **Điều khiển từ xa qua Telegram** — Bật/tắt task và nhận thông báo ngay trên điện thoại.

## Yêu cầu

- **Go 1.21+** để build.
- **Windows** cho cửa sổ desktop native (dùng runtime WebView2, có sẵn trên Windows 11). Trên OS khác — hoặc khi đặt `ZYPER_HEADLESS=1` — ứng dụng phục vụ cùng dashboard đó trên trình duyệt.

## Build & Chạy

```bash
# Build desktop Windows — KHÔNG hiện cửa sổ console, chỉ hiện UI (Go thuần, không CGO).
# -H windowsgui đặt GUI subsystem; log vẫn ghi vào thư mục logs/.
CGO_ENABLED=0 go build -ldflags="-H windowsgui" -o zyper-bot.exe ./cmd/server
# (hoặc chạy ./build.ps1)

# chạy — mở cửa sổ desktop native
./zyper-bot.exe
```

> Build mà KHÔNG có `-ldflags="-H windowsgui"` sẽ tạo exe kèm console (in log ra
> terminal) — tiện debug, nhưng sẽ hiện cửa sổ console.

Ứng dụng lưu dữ liệu ngay cạnh file thực thi: `zyperbot.db` (SQLite), `vault.key`, `logs/`, và đọc `.env` từ cùng thư mục. Giữ thư mục `web/` bên cạnh binary.

## Cấu hình

Mọi secret nằm trong file **`.env`** cục bộ (đã gitignore) và/hoặc tab **Settings** trong app — không bao giờ nằm trong source. Sao chép template:

```bash
cp .env.example .env
```

| Biến | Công dụng |
| --- | --- |
| `ZYPER_ETH_RPC` | URL RPC Ethereum mainnet mặc định. |
| `OPENSEA_API_KEY` | Một hoặc nhiều key OpenSea (cách nhau bằng xuống dòng/dấu phẩy) — tự xoay vòng. |
| `ETHERSCAN_API_KEY` | Key Etherscan V2 (đa chain) cho fetch ABI + replay giao dịch. |
| `ZYPER_AUTH_TOKEN` | Secret bắt buộc khi truy cập API **từ xa** (gửi qua header `X-Auth-Token`). |
| `ZYPER_HEADLESS` | `1` = chỉ chạy server và mở trên trình duyệt (chế độ VPS). |
| `ZYPER_ADDR` | Địa chỉ bind (mặc định `127.0.0.1:0`, cổng loopback ngẫu nhiên). |

Key đặt trong giao diện Settings được lưu vào DB và sẽ ghi đè `.env` ở lần khởi động sau.

## Mô hình bảo mật

- Private key được niêm phong bằng vault trước khi chạm tới ổ đĩa; bản rõ chỉ tồn tại trong bộ nhớ khi task đang chạy.
- Dashboard mặc định bind vào **cổng loopback ngẫu nhiên** — đây là kênh truyền cho cửa sổ nội bộ, không phải trang web công khai.
- **Truy cập từ xa bị từ chối** nếu chưa đặt `ZYPER_AUTH_TOKEN` (và nên đặt sau một reverse proxy có TLS).
- Endpoint hiện private key mặc định tắt (`ZYPER_ALLOW_REVEAL`) và chỉ cho loopback.
- **Không bao giờ commit `.env`, `vault.key`, hay `*.db`.** Chúng đã được gitignore.

## Công nghệ

Go · [go-ethereum](https://github.com/ethereum/go-ethereum) · [modernc.org/sqlite](https://gitlab.com/cznic/sqlite) (thuần Go) · router [chi](https://github.com/go-chi/chi) · [go-webview2](https://github.com/jchv/go-webview2) · frontend HTML/CSS/JS thuần.

## Giấy phép

Dùng cho mục đích cá nhân và học tập. Cung cấp nguyên trạng, không kèm bảo hành dưới bất kỳ hình thức nào.
