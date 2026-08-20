# netcfg-web

**Tiếng Việt** · [English](README.en.md)

Giao diện web cấu hình mạng cho thiết bị Debian headless: Wi-Fi qua đúng subsystem đang giữ radio (`wpa_supplicant` hoặc NetworkManager), địa chỉ IPv4/IPv6 qua đúng backend đang quản lý máy, và nhiều lớp an toàn để bạn không bao giờ tự khoá mình khỏi thiết bị.

Hai binary Go tĩnh, không phụ thuộc thư viện ngoài. Giao diện nhúng sẵn trong binary, hỗ trợ **tiếng Việt (mặc định)** và tiếng Anh.

---

## Ba đặc tính cốt lõi

### 1. Không thể tự khoá — commit–confirm

Mọi thay đổi có thể cắt đứt kết nối đều được áp dụng kèm một hẹn giờ khôi phục. Không xác nhận kịp thì thiết bị tự quay về cấu hình cũ.

```
POST /api/v1/plans        → xem trước diff, biết trước có nguy hiểm không
POST /api/v1/ip           → áp dụng, agent hẹn giờ 90 giây
POST .../confirm          → giữ thay đổi
(im lặng)                 → tự khôi phục
```

Hẹn giờ nằm trong tiến trình **root**, không phải trong trình duyệt hay tầng web, nên nó vẫn chạy kể cả khi cả hai đã chết. Thay đổi vô hại như đổi DNS được commit ngay, không hỏi.

### 2. Phân tách đặc quyền

```
Trình duyệt ──HTTPS──► netcfg-web        user netcfg, không capability
                            │
                       Unix socket + SO_PEERCRED
                            ▼
                       netcfgd           root, CAP_NET_ADMIN
```

Toàn bộ phần tiếp xúc với mạng — TLS, HTTP, cookie, template — chạy không đặc quyền. Tiến trình root chỉ nhận 23 lệnh đã định kiểu chặt.

### 3. AP dự phòng — lớp an toàn cuối cùng

Khi không còn đường mạng nào, thiết bị tự phát Wi-Fi riêng kèm captive portal. Cắm điện, mở điện thoại, cấu hình lại — không cần mang màn hình tới hiện trường.

---

## Failover giữa dây và Wi-Fi

Thiết bị cắm cả Ethernet lẫn Wi-Fi thì thứ tự ưu tiên do **route metric** quyết định: nhân chọn default route có metric thấp nhất và tự chuyển sang đường kế tiếp khi link đó rớt.

```
default via 192.168.2.1 dev eth0  metric 100   ← dùng khi có dây
default via 192.168.2.1 dev wlan0 metric 600   ← dự phòng
```

Mục **Failover** trên thanh menu liệt kê mọi interface theo thứ tự ưu tiên hiện tại, mỗi dòng có ô metric riêng, trạng thái lên/xuống và tùy chọn loại interface đó khỏi danh sách dự phòng. Giá trị được ghi xuống đúng backend đang quản lý máy — `RouteMetric=` của systemd-networkd, `ipv4.route-metric` của NetworkManager, `metric` của ifupdown. Đổi metric được xem là thay đổi nguy hiểm nên vẫn đi qua commit–confirm.

Tầng nhân chỉ phản ứng khi link *down*. Phần còn lại do **theo dõi chủ động** đảm nhiệm: `netcfgd` ping qua từng interface (ràng socket vào đúng interface đó), hỏng ba lần liên tiếp thì đẩy default route của nó xuống cuối bảng định tuyến, thông lại hai lần thì trả về đúng metric cũ.

```
Bộ theo dõi                    Ràng buộc an toàn
mỗi 10 giây, ping từng đường   không bao giờ hạ đường cuối cùng còn sống
3 lần hỏng  → hạ ưu tiên       chỉ đụng bảng định tuyến trong nhân,
2 lần thông → khôi phục          không ghi vào file cấu hình nào
```

Mặc định nó thử gateway của chính interface đó. Muốn bắt được cả trường hợp gateway còn sống nhưng phía trên đã đứt thì trỏ ra ngoài: `netcfgd -probe-targets 1.1.1.1:53`. Tắt hẳn bằng `-failover-monitor=false`.

---

## Bắt đầu nhanh

### Cài bằng gói .deb

Một file, một lệnh; nâng cấp và gỡ bỏ đều qua apt:

```sh
# apt tải file bằng user _apt, không đọc được /root
cd /tmp

base=https://github.com/thanhnhu/netcfg/releases/download/latest
curl -fsSLO "$base/SHA256SUMS"
# Tên file .deb mang số phiên bản nên lấy thẳng từ SHA256SUMS
deb=$(awk '/arm64\.deb$/ {print $2}' SHA256SUMS)   # hoặc amd64 / armhf
curl -fsSLO "$base/$deb"
grep "$deb" SHA256SUMS | sha256sum -c -

sudo apt install "./$deb"
```

Gói tự hỏi mật khẩu quản trị, hỏi có cho phép giao diện bật/tắt SSH hay không
(mặc định không), tạo user hệ thống `netcfg`, cài và bật hai unit systemd, đồng
thời kéo theo `wpasupplicant`, `hostapd`, `dnsmasq`, `iw` ở mức Recommends.
Binary nằm ở `/usr/bin`, nên đừng chạy thêm `install.sh` trên cùng máy — script
đó ghi một bản thứ hai vào `/usr/local/bin`. Đổi ý sau này thì
`sudo dpkg-reconfigure netcfg`.

Không nhập mật khẩu (hoặc cài tự động, không có màn hình) thì `netcfgd` vẫn chạy,
riêng `netcfg-web` chờ tới khi bạn đặt:

```sh
sudo netcfg-web -set-password -username admin -config /etc/netcfg-web/config.json
sudo systemctl enable --now netcfg-web
```

Nâng cấp: cài đè file `.deb` mới theo đúng cách trên. `apt remove netcfg` dừng và
gỡ dịch vụ nhưng giữ mật khẩu, phiên đăng nhập và lịch sử; `apt purge netcfg` xoá
luôn những thứ đó.

### Cài từ bản phát hành

Chạy trên chính thiết bị, không cần Go và không cần máy dev:

```sh
apt install wpasupplicant iproute2 hostapd dnsmasq iw

mkdir -p ~/netcfg-install && cd ~/netcfg-install
base=https://github.com/thanhnhu/netcfg/releases/download/latest
curl -fsSLO "$base/netcfg-latest-linux-arm64.tar.gz"    # hoặc amd64 / armv7
curl -fsSLO "$base/SHA256SUMS"
grep linux-arm64 SHA256SUMS | sha256sum -c -

tar -xzf netcfg-*-linux-arm64.tar.gz && cd netcfg-*-linux-arm64
cat VERSION                  # tag, commit và thời điểm build của gói này
sudo sh deploy/install.sh .
```

Đừng bỏ bước `sha256sum`: gói này chứa binary sẽ chạy với quyền root.

Đường dẫn là `/releases/download/latest/`, không phải `/releases/latest/download/`
— dạng thứ hai bỏ qua prerelease, mà bản rolling chính là prerelease.

Muốn bản đã đánh phiên bản thì lấy tên file từ
[trang Releases](https://github.com/thanhnhu/netcfg/releases) — tên gói chứa số
phiên bản, ví dụ `netcfg-v0.1.0-linux-arm64.tar.gz`.

Ngoài các bản `v*`, mỗi lần build tay có thể phát hành một prerelease tên `latest`
— tiện để thử nhanh, nhưng nó bị thay thế bất cứ lúc nào nên đừng dùng cho máy
chạy thật.

### Build từ mã nguồn

Trên máy dev:

```sh
make arm64                 # hoặc: make amd64 / make armv7
```

`make deb-arm64` (và `deb-amd64`, `deb-armv7`) đóng chính những binary đó thành
gói `.deb`. Lệnh này cần `dpkg-deb`, nên chạy trên máy Debian hoặc trong WSL.

Trên thiết bị Debian:

```sh
apt install wpasupplicant iproute2 hostapd dnsmasq iw
scp dist/netcfgd dist/netcfg-web deploy/*.service deploy/install.sh pi@thiet-bi:~/
sudo ./install.sh .
```

Dù đi đường nào, script cũng tạo user hệ thống, cài hai unit systemd, hỏi mật khẩu quản trị, kiểm tra điều kiện tiên quyết và in ra danh sách URL truy cập.

**Bắt buộc** — `/etc/wpa_supplicant/wpa_supplicant-wlan0.conf` phải có:

```
ctrl_interface=DIR=/run/wpa_supplicant GROUP=netdev
update_config=1
country=VN
```

Thiếu `update_config=1` thì Wi-Fi vẫn kết nối được nhưng mất sau khi khởi động lại (ứng dụng sẽ cảnh báo).

Máy dùng NetworkManager thì bỏ qua phần này: ứng dụng tự nhận ra và điều khiển radio qua `nmcli`.

### Nâng cấp

Chép đè binary và unit rồi khởi động lại — giữ nguyên mật khẩu, phiên đăng nhập và cấu hình đã lưu:

```sh
sudo install -m 0755 netcfgd netcfg-web /usr/local/bin/
sudo install -m 0644 deploy/netcfgd.service deploy/netcfg-web.service /etc/systemd/system/
sudo systemctl daemon-reload
sudo systemctl restart netcfgd netcfg-web
systemctl is-active netcfgd netcfg-web
```

Đừng bỏ qua hai file `.service`: bản sửa nằm trong đó chứ không chỉ trong binary.

### Gỡ cài đặt

Cài bằng `.deb`:

```sh
sudo apt purge netcfg
```

Cài bằng `install.sh`:

```sh
sudo systemctl disable --now netcfgd netcfg-web
sudo rm -f /etc/systemd/system/netcfgd.service /etc/systemd/system/netcfg-web.service
sudo systemctl daemon-reload && sudo systemctl reset-failed

sudo rm -f /usr/local/bin/netcfgd /usr/local/bin/netcfg-web
sudo rm -rf /etc/netcfg-web        # mật khẩu quản trị
sudo rm -rf /var/lib/netcfg-web    # phiên đăng nhập, chứng chỉ TLS
sudo rm -rf /var/lib/netcfgd       # desired.json, last-known-good, lịch sử
sudo rm -rf /run/netcfgd

sudo userdel netcfg
sudo groupdel netcfg
```

Kiểm tra không còn gì:

```sh
systemctl list-units --all 'netcfg*'
getent passwd netcfg; getent group netcfg
```

**Gỡ cài đặt không hoàn tác cấu hình mạng.** Những gì đã ghi vào `/etc/systemd/network/`
vẫn nguyên, máy vẫn chạy với metric và địa chỉ bạn đã đặt. Trong lúc cài,
`install.sh` cũng đã `systemctl disable hostapd dnsmasq`; nếu trước đó bạn dùng
chúng cho việc khác thì bật lại bằng tay.

---

## Truy cập qua LAN

Mặc định lắng nghe `:8090` trên **mọi** interface với HTTPS, chứng chỉ tự ký sinh tự động chứa hostname, `<hostname>.local` và mọi IP của máy.

```sh
https://192.168.1.50:8090/          # bằng IP
https://thiet-bi.local:8090/        # cần avahi-daemon

journalctl -u netcfg-web | grep fingerprint   # đối chiếu khi trình duyệt cảnh báo
ufw allow from 192.168.0.0/16 to any port 8090 proto tcp
```

Chi tiết mDNS, reverse proxy và tường lửa: [docs/deployment.md](docs/deployment.md).

---

## Tài khoản quản trị

Một tài khoản duy nhất, mật khẩu lưu dưới dạng PBKDF2-HMAC-SHA256 240.000 vòng kèm salt riêng. Đặt lần đầu bằng `install.sh`, hoặc bất cứ lúc nào:

```sh
netcfg-web -set-password -username admin -config /etc/netcfg-web/config.json
```

Từ giao diện, bấm tên người dùng ở góc trên bên phải → **Đổi mật khẩu**. Đổi xong, mọi phiên đăng nhập khác bị thu hồi, riêng phiên đang thao tác được giữ lại để bạn không tự đá mình ra ngoài. Đoán mật khẩu hiện tại ở đây bị chặn bằng cùng bộ đếm giới hạn với trang đăng nhập.

---

## Tài liệu

| Tài liệu | Nội dung |
|---|---|
| [docs/architecture.md](docs/architecture.md) | Phân lớp hexagonal, luồng dữ liệu, cấu trúc package, bảng quyết định |
| [docs/networking.md](docs/networking.md) | Ba backend IP, dual-stack IPv4/IPv6, failover theo route metric, AP dự phòng & captive portal |
| [docs/api.md](docs/api.md) | Tham chiếu API v1, SSE, định dạng lỗi RFC 9457 |
| [docs/deployment.md](docs/deployment.md) | Cài đặt, systemd, tham số dòng lệnh, truy cập LAN, gỡ rối |
| [docs/security.md](docs/security.md) | Mô hình mối đe doạ và các biện pháp đối phó |
| [docs/development.md](docs/development.md) | Build, test, fuzz, quy ước đóng góp |

Tài liệu trong `docs/` viết bằng tiếng Anh vì hướng tới lập trình viên; giao diện dành cho người vận hành thì mặc định tiếng Việt.

---

## Trạng thái

| Hạng mục | Trạng thái |
|---|---|
| Wi-Fi: quét, kết nối, lưu/xoá profile, WPA2/WPA3 | Xong |
| IPv4 tĩnh/DHCP, IPv6 tĩnh/auto/tắt | Xong |
| Failover dây/Wi-Fi bằng route metric | Xong |
| Failover chủ động (theo dõi bằng ping) | Xong |
| Backend IP: systemd-networkd, NetworkManager, ifupdown | Xong |
| Backend Wi-Fi: wpa_supplicant, NetworkManager | Xong |
| Commit–confirm và tự khôi phục | Xong |
| AP dự phòng kèm captive portal | Xong |
| Phiên đăng nhập sống sót qua restart | Xong |
| Thông số hệ thống: CPU, RAM, đĩa, cảm biến tự phát hiện | Xong |
| Đổi mật khẩu quản trị từ giao diện | Xong |
| Giao diện đa ngôn ngữ (vi/en) | Xong |
| VLAN, bonding, bridge | Chưa |
| Đa người dùng, phân quyền theo vai trò | Chưa |
