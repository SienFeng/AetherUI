const Protocols = {
    VMESS: 'vmess',
    VLESS: 'vless',
    TROJAN: 'trojan',
    SHADOWSOCKS: 'shadowsocks',
    DOKODEMO: 'dokodemo-door',
    MTPROTO: 'mtproto',
    SOCKS: 'socks',
    HTTP: 'http',
};

const VmessMethods = {
    AES_128_GCM: 'aes-128-gcm',
    CHACHA20_POLY1305: 'chacha20-poly1305',
    AUTO: 'auto',
    NONE: 'none',
};

const SSMethods = {
    // AES_256_CFB: 'aes-256-cfb',
    // AES_128_CFB: 'aes-128-cfb',
    // CHACHA20: 'chacha20',
    // CHACHA20_IETF: 'chacha20-ietf',
    CHACHA20_POLY1305: 'chacha20-poly1305',
    AES_256_GCM: 'aes-256-gcm',
    AES_128_GCM: 'aes-128-gcm',
};

const RULE_IP = {
    PRIVATE: 'geoip:private',
    CN: 'geoip:cn',
};

const RULE_DOMAIN = {
    ADS: 'geosite:category-ads',
    ADS_ALL: 'geosite:category-ads-all',
    CN: 'geosite:cn',
    GOOGLE: 'geosite:google',
    FACEBOOK: 'geosite:facebook',
    SPEEDTEST: 'geosite:speedtest',
};

// 当前 Xray 核心（infra/conf/vless.go:51）只接受 "" 与 xtls-rprx-vision。
// 旧的 xtls-rprx-origin / xtls-rprx-direct 已被移除，填了会让整份配置加载失败。
const FLOW_CONTROL = {
    VISION: "xtls-rprx-vision",
};

const TLS_VERSION_OPTION = ["1.0", "1.1", "1.2", "1.3"];

// 只列真正生效的 TLS 1.2 套件。核心把这个串按 ":" 切开逐个查表，
// 查不到的**静默丢弃**（transport/internet/tls/config.go:459-463，没有
// else 分支），所以界面必须是下拉多选而不是自由文本框。
// 另外 Go 的 crypto/tls 不接受 TLS 1.3 的套件配置——Vision 与 REALITY 都走
// 1.3，这一项对它们完全无效。
const TLS_CIPHER_OPTION = [
    "TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256",
    "TLS_ECDHE_ECDSA_WITH_AES_256_GCM_SHA384",
    "TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256",
    "TLS_ECDHE_RSA_WITH_AES_256_GCM_SHA384",
    "TLS_ECDHE_RSA_WITH_CHACHA20_POLY1305_SHA256",
    "TLS_ECDHE_ECDSA_WITH_CHACHA20_POLY1305_SHA256",
];

// 不含 unsafe / hellogolang：核心在 transport_security.go:181 拒绝这两个值。
// 注意那段校验只在 REALITY 作为**出站**时生效，入站侧核心根本不读这个字段，
// 所以 xray run -test 验不出来，只能靠这个列表挡住。
const UTLS_FINGERPRINT = [
    "chrome", "firefox", "safari", "ios", "android", "edge", "360", "qq",
    "random", "randomized",
];

const ALPN_OPTION = ["h3", "h2", "http/1.1"];

const SNIFFING_OPTION = ["http", "tls", "quic", "fakedns"];

// 伪装目标候选。四项判据（TLS1.3 / ALPN h2 / X25519 系密钥交换 / 证书有效）
// 已于 2026-09-03 逐个实测确认，且均不命中核心的高风险判定
// （transport_security.go:164-170：.ru/.ir/.cn 后缀，含 apple/icloud/microsoft）。
// 五个域名当时都协商出 X25519MLKEM768，满足 3x-ui reality_scan.go:298 的判据。
// 复核方式见本计划 Task 4 Step 1；域名的 TLS 配置会变，隔一段时间要重测。
//
// player.twitch.tv 曾是候选，因协商不出 ALPN h2 被淘汰——不要再加回来。
const REALITY_TARGET_PRESETS = [
    "www.lovelive-anime.jp",
    "www.amazon.co.jp",
    "www.tesla.com",
    "www.cloudflare.com",
    "www.nicovideo.jp",
];

Object.freeze(Protocols);
Object.freeze(VmessMethods);
Object.freeze(SSMethods);
Object.freeze(RULE_IP);
Object.freeze(RULE_DOMAIN);
Object.freeze(FLOW_CONTROL);
Object.freeze(TLS_VERSION_OPTION);
Object.freeze(TLS_CIPHER_OPTION);
Object.freeze(UTLS_FINGERPRINT);
Object.freeze(ALPN_OPTION);
Object.freeze(SNIFFING_OPTION);
Object.freeze(REALITY_TARGET_PRESETS);

class XrayCommonClass {

    static toJsonArray(arr) {
        return arr.map(obj => obj.toJson());
    }

    static fromJson() {
        return new XrayCommonClass();
    }

    toJson() {
        return this;
    }

    toString(format=true) {
        return format ? JSON.stringify(this.toJson(), null, 2) : JSON.stringify(this.toJson());
    }

    static toHeaders(v2Headers) {
        let newHeaders = [];
        if (v2Headers) {
            Object.keys(v2Headers).forEach(key => {
                let values = v2Headers[key];
                if (typeof(values) === 'string') {
                    newHeaders.push({ name: key, value: values });
                } else {
                    for (let i = 0; i < values.length; ++i) {
                        newHeaders.push({ name: key, value: values[i] });
                    }
                }
            });
        }
        return newHeaders;
    }

    static toV2Headers(headers, arr=true) {
        let v2Headers = {};
        for (let i = 0; i < headers.length; ++i) {
            let name = headers[i].name;
            let value = headers[i].value;
            if (ObjectUtil.isEmpty(name) || ObjectUtil.isEmpty(value)) {
                continue;
            }
            if (!(name in v2Headers)) {
                v2Headers[name] = arr ? [value] : value;
            } else {
                if (arr) {
                    v2Headers[name].push(value);
                } else {
                    v2Headers[name] = value;
                }
            }
        }
        return v2Headers;
    }
}

class TcpStreamSettings extends XrayCommonClass {
    constructor(type='none',
                request=new TcpStreamSettings.TcpRequest(),
                response=new TcpStreamSettings.TcpResponse(),
                ) {
        super();
        this.type = type;
        this.request = request;
        this.response = response;
    }

    static fromJson(json={}) {
        let header = json.header;
        if (!header) {
            header = {};
        }
        return new TcpStreamSettings(
            header.type,
            TcpStreamSettings.TcpRequest.fromJson(header.request),
            TcpStreamSettings.TcpResponse.fromJson(header.response),
        );
    }

    toJson() {
        return {
            header: {
                type: this.type,
                request: this.type === 'http' ? this.request.toJson() : undefined,
                response: this.type === 'http' ? this.response.toJson() : undefined,
            },
        };
    }
}

TcpStreamSettings.TcpRequest = class extends XrayCommonClass {
    constructor(version='1.1',
                method='GET',
                path=['/'],
                headers=[],
    ) {
        super();
        this.version = version;
        this.method = method;
        this.path = path.length === 0 ? ['/'] : path;
        this.headers = headers;
    }

    addPath(path) {
        this.path.push(path);
    }

    removePath(index) {
        this.path.splice(index, 1);
    }

    addHeader(name, value) {
        this.headers.push({ name: name, value: value });
    }

    getHeader(name) {
        for (const header of this.headers) {
            if (header.name.toLowerCase() === name.toLowerCase()) {
                return header.value;
            }
        }
        return null;
    }

    removeHeader(index) {
        this.headers.splice(index, 1);
    }

    static fromJson(json={}) {
        return new TcpStreamSettings.TcpRequest(
            json.version,
            json.method,
            json.path,
            XrayCommonClass.toHeaders(json.headers),
        );
    }

    toJson() {
        return {
            method: this.method,
            path: ObjectUtil.clone(this.path),
            headers: XrayCommonClass.toV2Headers(this.headers),
        };
    }
};

TcpStreamSettings.TcpResponse = class extends XrayCommonClass {
    constructor(version='1.1',
                status='200',
                reason='OK',
                headers=[],
    ) {
        super();
        this.version = version;
        this.status = status;
        this.reason = reason;
        this.headers = headers;
    }

    addHeader(name, value) {
        this.headers.push({ name: name, value: value });
    }

    removeHeader(index) {
        this.headers.splice(index, 1);
    }

    static fromJson(json={}) {
        return new TcpStreamSettings.TcpResponse(
            json.version,
            json.status,
            json.reason,
            XrayCommonClass.toHeaders(json.headers),
        );
    }

    toJson() {
        return {
            version: this.version,
            status: this.status,
            reason: this.reason,
            headers: XrayCommonClass.toV2Headers(this.headers),
        };
    }
};

class KcpStreamSettings extends XrayCommonClass {
    constructor(mtu=1350, tti=20,
                uplinkCapacity=5,
                downlinkCapacity=20,
                congestion=false,
                readBufferSize=2,
                writeBufferSize=2,
                type='none',
                seed=RandomUtil.randomSeq(10),
                ) {
        super();
        this.mtu = mtu;
        this.tti = tti;
        this.upCap = uplinkCapacity;
        this.downCap = downlinkCapacity;
        this.congestion = congestion;
        this.readBuffer = readBufferSize;
        this.writeBuffer = writeBufferSize;
        this.type = type;
        this.seed = seed;
    }

    static fromJson(json={}) {
        return new KcpStreamSettings(
            json.mtu,
            json.tti,
            json.uplinkCapacity,
            json.downlinkCapacity,
            json.congestion,
            json.readBufferSize,
            json.writeBufferSize,
            ObjectUtil.isEmpty(json.header) ? 'none' : json.header.type,
            json.seed,
        );
    }

    toJson() {
        return {
            mtu: this.mtu,
            tti: this.tti,
            uplinkCapacity: this.upCap,
            downlinkCapacity: this.downCap,
            congestion: this.congestion,
            readBufferSize: this.readBuffer,
            writeBufferSize: this.writeBuffer,
            header: {
                type: this.type,
            },
            seed: this.seed,
        };
    }
}

class WsStreamSettings extends XrayCommonClass {
    constructor(path='/', headers=[]) {
        super();
        this.path = path;
        this.headers = headers;
    }

    addHeader(name, value) {
        this.headers.push({ name: name, value: value });
    }

    getHeader(name) {
        for (const header of this.headers) {
            if (header.name.toLowerCase() === name.toLowerCase()) {
                return header.value;
            }
        }
        return null;
    }

    removeHeader(index) {
        this.headers.splice(index, 1);
    }

    static fromJson(json={}) {
        return new WsStreamSettings(
            json.path,
            XrayCommonClass.toHeaders(json.headers),
        );
    }

    toJson() {
        return {
            path: this.path,
            headers: XrayCommonClass.toV2Headers(this.headers, false),
        };
    }
}

class GrpcStreamSettings extends XrayCommonClass {
    constructor(serviceName="") {
        super();
        this.serviceName = serviceName;
    }

    static fromJson(json={}) {
        return new GrpcStreamSettings(json.serviceName);
    }

    toJson() {
        return {
            serviceName: this.serviceName,
        }
    }
}

class TlsStreamSettings extends XrayCommonClass {
    constructor(serverName='',
                minVersion='1.2',
                maxVersion='1.3',
                cipherSuites='',
                rejectUnknownSni=false,
                alpn=['h2', 'http/1.1'],
                echServerKeys='',
                certificates=[new TlsStreamSettings.Cert()],
                settings=new TlsStreamSettings.Settings()) {
        super();
        this.server = serverName;
        this.minVersion = minVersion;
        this.maxVersion = maxVersion;
        this.cipherSuites = cipherSuites;
        this.rejectUnknownSni = rejectUnknownSni;
        this.alpn = alpn;
        this.echServerKeys = echServerKeys;
        this.certs = certificates;
        this.settings = settings;
    }

    addCert(cert) {
        this.certs.push(cert);
    }

    removeCert(index) {
        this.certs.splice(index, 1);
    }

    static fromJson(json={}) {
        let certs;
        if (!ObjectUtil.isEmpty(json.certificates)) {
            certs = json.certificates.map(cert => TlsStreamSettings.Cert.fromJson(cert));
        }
        return new TlsStreamSettings(
            json.serverName,
            json.minVersion,
            json.maxVersion,
            json.cipherSuites,
            json.rejectUnknownSni,
            json.alpn,
            json.echServerKeys,
            certs,
            TlsStreamSettings.Settings.fromJson(json.settings),
        );
    }

    toJson() {
        return {
            serverName: this.server,
            minVersion: this.minVersion,
            maxVersion: this.maxVersion,
            cipherSuites: this.cipherSuites,
            rejectUnknownSni: this.rejectUnknownSni,
            alpn: this.alpn,
            echServerKeys: this.echServerKeys,
            certificates: TlsStreamSettings.toJsonArray(this.certs),
            settings: this.settings.toJson(),
        };
    }
}

// settings 是**面板私有**的客户端半边参数，核心的 TLSConfig 里没有这个键。
// 已实测确认核心忽略它而不是拒绝（web/service/inbound_stream_contract_test.go
// 的 TestPanelOnlySettingsKeyIsIgnoredByCore）。存在这里是为了让分享链接
// 能带上 fp / ech 两个参数——它们是客户端要用的，服务端不读。
TlsStreamSettings.Settings = class extends XrayCommonClass {
    constructor(fingerprint='chrome', allowInsecure=false, echConfigList='') {
        super();
        this.fingerprint = fingerprint;
        this.allowInsecure = allowInsecure;
        this.echConfigList = echConfigList;
    }

    static fromJson(json={}) {
        if (ObjectUtil.isEmpty(json)) {
            return new TlsStreamSettings.Settings();
        }
        return new TlsStreamSettings.Settings(
            json.fingerprint,
            json.allowInsecure,
            json.echConfigList,
        );
    }

    toJson() {
        return {
            fingerprint: this.fingerprint,
            allowInsecure: this.allowInsecure,
            echConfigList: this.echConfigList,
        };
    }
};

TlsStreamSettings.Cert = class extends XrayCommonClass {
    constructor(useFile=true, certificateFile='', keyFile='', certificate='', key='', ocspStapling=3600) {
        super();
        this.useFile = useFile;
        this.certFile = certificateFile;
        this.keyFile = keyFile;
        this.cert = certificate instanceof Array ? certificate.join('\n') : certificate;
        this.key = key instanceof Array ? key.join('\n') : key;
        this.ocspStapling = ocspStapling;
    }

    static fromJson(json={}) {
        if ('certificateFile' in json && 'keyFile' in json) {
            return new TlsStreamSettings.Cert(
                true,
                json.certificateFile,
                json.keyFile,
                '', '',
                json.ocspStapling,
            );
        } else {
            return new TlsStreamSettings.Cert(
                false, '', '',
                json.certificate.join('\n'),
                json.key.join('\n'),
                json.ocspStapling,
            );
        }
    }

    toJson() {
        if (this.useFile) {
            return {
                certificateFile: this.certFile,
                keyFile: this.keyFile,
                ocspStapling: this.ocspStapling,
            };
        } else {
            return {
                certificate: this.cert.split('\n'),
                key: this.key.split('\n'),
                ocspStapling: this.ocspStapling,
            };
        }
    }
};

class StreamSettings extends XrayCommonClass {
    constructor(network='tcp',
                security='none',
                tlsSettings=new TlsStreamSettings(),
                tcpSettings=new TcpStreamSettings(),
                kcpSettings=new KcpStreamSettings(),
                wsSettings=new WsStreamSettings(),
                grpcSettings=new GrpcStreamSettings(),
                ) {
        super();
        this.network = network;
        this.security = security;
        this.tls = tlsSettings;
        this.tcp = tcpSettings;
        this.kcp = kcpSettings;
        this.ws = wsSettings;
        this.grpc = grpcSettings;
    }

    get isTls() {
        return this.security === 'tls';
    }

    set isTls(isTls) {
        if (isTls) {
            this.security = 'tls';
        } else {
            this.security = 'none';
        }
    }

    get isXTls() {
        return this.security === "xtls";
    }

    set isXTls(isXTls) {
        if (isXTls) {
            this.security = 'xtls';
        } else {
            this.security = 'none';
        }
    }

    static fromJson(json={}) {
        let tls = TlsStreamSettings.fromJson(json.tlsSettings);
        return new StreamSettings(
            json.network,
            json.security,
            tls,
            TcpStreamSettings.fromJson(json.tcpSettings),
            KcpStreamSettings.fromJson(json.kcpSettings),
            WsStreamSettings.fromJson(json.wsSettings),
            GrpcStreamSettings.fromJson(json.grpcSettings),
        );
    }

    toJson() {
        const network = this.network;
        return {
            network: network,
            security: this.security,
            tlsSettings: this.isTls ? this.tls.toJson() : undefined,
            tcpSettings: network === 'tcp' ? this.tcp.toJson() : undefined,
            kcpSettings: network === 'kcp' ? this.kcp.toJson() : undefined,
            wsSettings: network === 'ws' ? this.ws.toJson() : undefined,
            grpcSettings: network === 'grpc' ? this.grpc.toJson() : undefined,
        };
    }
}

class Sniffing extends XrayCommonClass {
    constructor(enabled=true, destOverride=['http', 'tls']) {
        super();
        this.enabled = enabled;
        this.destOverride = destOverride;
    }

    static fromJson(json={}) {
        let destOverride = ObjectUtil.clone(json.destOverride);
        if (!ObjectUtil.isEmpty(destOverride) && !ObjectUtil.isArrEmpty(destOverride)) {
            if (ObjectUtil.isEmpty(destOverride[0])) {
                destOverride = ['http', 'tls'];
            }
        }
        return new Sniffing(
            !!json.enabled,
            destOverride,
        );
    }
}

class Inbound extends XrayCommonClass {
    constructor(port=RandomUtil.randomIntRange(10000, 60000),
                listen='',
                protocol=Protocols.VMESS,
                settings=null,
                streamSettings=new StreamSettings(),
                tag='',
                sniffing=new Sniffing(),
                ) {
        super();
        this.port = port;
        this.listen = listen;
        this._protocol = protocol;
        this.settings = ObjectUtil.isEmpty(settings) ? Inbound.Settings.getSettings(protocol) : settings;
        this.stream = streamSettings;
        this.tag = tag;
        this.sniffing = sniffing;
    }

    get protocol() {
        return this._protocol;
    }

    set protocol(protocol) {
        this._protocol = protocol;
        this.settings = Inbound.Settings.getSettings(protocol);
        if (protocol === Protocols.TROJAN) {
            this.tls = true;
        }
    }

    get tls() {
        return this.stream.security === 'tls';
    }

    set tls(isTls) {
        if (isTls) {
            this.stream.security = 'tls';
        } else {
            if (this.protocol === Protocols.TROJAN) {
                this.xtls = true;
            } else {
                this.stream.security = 'none';
            }
        }
    }

    get xtls() {
        return this.stream.security === 'xtls';
    }

    set xtls(isXTls) {
        if (isXTls) {
            this.stream.security = 'xtls';
        } else {
            if (this.protocol === Protocols.TROJAN) {
                this.tls = true;
            } else {
                this.stream.security = 'none';
            }
        }
    }

    get network() {
        return this.stream.network;
    }

    set network(network) {
        this.stream.network = network;
    }

    get isTcp() {
        return this.network === "tcp";
    }

    get isWs() {
        return this.network === "ws";
    }

    get isKcp() {
        return this.network === "kcp";
    }

    get isGrpc() {
        return this.network === "grpc";
    }

    get isH2() {
        return this.network === "http";
    }

    // VMess & VLess
    get uuid() {
        switch (this.protocol) {
            case Protocols.VMESS:
                return this.settings.vmesses[0].id;
            case Protocols.VLESS:
                return this.settings.vlesses[0].id;
            default:
                return "";
        }
    }

    // VLess & Trojan
    get flow() {
        switch (this.protocol) {
            case Protocols.VLESS:
                return this.settings.vlesses[0].flow;
            case Protocols.TROJAN:
                return this.settings.clients[0].flow;
            default:
                return "";
        }
    }

    // VMess
    get alterId() {
        switch (this.protocol) {
            case Protocols.VMESS:
                return this.settings.vmesses[0].alterId;
            default:
                return "";
        }
    }

    // Socks & HTTP
    get username() {
        switch (this.protocol) {
            case Protocols.SOCKS:
            case Protocols.HTTP:
                return this.settings.accounts[0].user;
            default:
                return "";
        }
    }

    // Trojan & Shadowsocks & Socks & HTTP
    get password() {
        switch (this.protocol) {
            case Protocols.TROJAN:
                return this.settings.clients[0].password;
            case Protocols.SHADOWSOCKS:
                return this.settings.password;
            case Protocols.SOCKS:
            case Protocols.HTTP:
                return this.settings.accounts[0].pass;
            default:
                return "";
        }
    }

    // Shadowsocks
    get method() {
        switch (this.protocol) {
            case Protocols.SHADOWSOCKS:
                return this.settings.method;
            default:
                return "";
        }
    }

    get serverName() {
        if (this.stream.isTls || this.stream.isXTls) {
            return this.stream.tls.server;
        }
        return "";
    }

    get host() {
        if (this.isTcp) {
            return this.stream.tcp.request.getHeader("Host");
        } else if (this.isWs) {
            return this.stream.ws.getHeader("Host");
        }
        return null;
    }

    get path() {
        if (this.isTcp) {
            return this.stream.tcp.request.path[0];
        } else if (this.isWs) {
            return this.stream.ws.path;
        }
        return null;
    }

    get kcpType() {
        return this.stream.kcp.type;
    }

    get kcpSeed() {
        return this.stream.kcp.seed;
    }

    get serviceName() {
        return this.stream.grpc.serviceName;
    }

    canEnableTls() {
        switch (this.protocol) {
            case Protocols.VMESS:
            case Protocols.VLESS:
            case Protocols.TROJAN:
            case Protocols.SHADOWSOCKS:
                break;
            default:
                return false;
        }

        switch (this.network) {
            case "tcp":
            case "ws":
            case "grpc":
                return true;
            default:
                return false;
        }
    }

    canSetTls() {
        return this.canEnableTls();
    }

    // REALITY 只支持 RAW(tcp) / XHTTP / gRPC（infra/conf/transport_internet.go:100）。
    // 本项目不做 XHTTP，因此只剩 tcp 与 grpc。
    canEnableReality() {
        switch (this.protocol) {
            case Protocols.VLESS:
            case Protocols.TROJAN:
                break;
            default:
                return false;
        }
        return this.network === "tcp" || this.network === "grpc";
    }

    // Vision 只对 VLESS 有效，且外层必须是 TLS 1.3 或 REALITY
    // （proxy/vless/inbound/inbound.go:573 在运行期检查，run -test 查不出来）。
    canEnableVision() {
        if (this.protocol !== Protocols.VLESS) {
            return false;
        }
        return this.stream.security === 'tls' || this.stream.security === 'reality';
    }

    canEnableXTls() {
        switch (this.protocol) {
            case Protocols.VLESS:
            case Protocols.TROJAN:
                break;
            default:
                return false;
        }
        return this.network === "tcp";
    }

    canEnableStream() {
        switch (this.protocol) {
            case Protocols.VMESS:
            case Protocols.VLESS:
            case Protocols.SHADOWSOCKS:
                return true;
            default:
                return false;
        }
    }

    canSniffing() {
        switch (this.protocol) {
            case Protocols.VMESS:
            case Protocols.VLESS:
            case Protocols.TROJAN:
            case Protocols.SHADOWSOCKS:
                return true;
            default:
                return false;
        }
    }

    reset() {
        this.port = RandomUtil.randomIntRange(10000, 60000);
        this.listen = '';
        this.protocol = Protocols.VMESS;
        this.settings = Inbound.Settings.getSettings(Protocols.VMESS);
        this.stream = new StreamSettings();
        this.tag = '';
        this.sniffing = new Sniffing();
    }

    genVmessLink(address='', remark='') {
        if (this.protocol !== Protocols.VMESS) {
            return '';
        }
        let network = this.stream.network;
        let type = 'none';
        let host = '';
        let path = '';
        if (network === 'tcp') {
            let tcp = this.stream.tcp;
            type = tcp.type;
            if (type === 'http') {
                let request = tcp.request;
                path = request.path.join(',');
                let index = request.headers.findIndex(header => header.name.toLowerCase() === 'host');
                if (index >= 0) {
                    host = request.headers[index].value;
                }
            }
        } else if (network === 'kcp') {
            let kcp = this.stream.kcp;
            type = kcp.type;
            path = kcp.seed;
        } else if (network === 'ws') {
            let ws = this.stream.ws;
            path = ws.path;
            let index = ws.headers.findIndex(header => header.name.toLowerCase() === 'host');
            if (index >= 0) {
                host = ws.headers[index].value;
            }
        } else if (network === 'http') {
            network = 'h2';
            path = this.stream.http.path;
            host = this.stream.http.host.join(',');
        } else if (network === 'quic') {
            type = this.stream.quic.type;
            host = this.stream.quic.security;
            path = this.stream.quic.key;
        } else if (network === 'grpc') {
            path = this.stream.grpc.serviceName;
        }

        if (this.stream.security === 'tls') {
            if (!ObjectUtil.isEmpty(this.stream.tls.server)) {
                address = this.stream.tls.server;
            }
        }

        let obj = {
            v: '2',
            ps: remark,
            add: address,
            port: this.port,
            id: this.settings.vmesses[0].id,
            aid: this.settings.vmesses[0].alterId,
            net: network,
            type: type,
            host: host,
            path: path,
            tls: this.stream.security,
        };
        return 'vmess://' + base64(JSON.stringify(obj, null, 2));
    }

    genVLESSLink(address = '', remark='') {
        const settings = this.settings;
        const uuid = settings.vlesses[0].id;
        const port = this.port;
        const type = this.stream.network;
        const params = new Map();
        params.set("type", this.stream.network);
        if (this.xtls) {
            params.set("security", "xtls");
        } else {
            params.set("security", this.stream.security);
        }
        switch (type) {
            case "tcp":
                const tcp = this.stream.tcp;
                if (tcp.type === 'http') {
                    const request = tcp.request;
                    params.set("path", request.path.join(','));
                    const index = request.headers.findIndex(header => header.name.toLowerCase() === 'host');
                    if (index >= 0) {
                        const host = request.headers[index].value;
                        params.set("host", host);
                    }
                }
                break;
            case "kcp":
                const kcp = this.stream.kcp;
                params.set("headerType", kcp.type);
                params.set("seed", kcp.seed);
                break;
            case "ws":
                const ws = this.stream.ws;
                params.set("path", ws.path);
                const index = ws.headers.findIndex(header => header.name.toLowerCase() === 'host');
                if (index >= 0) {
                    const host = ws.headers[index].value;
                    params.set("host", host);
                }
                break;
            case "http":
                const http = this.stream.http;
                params.set("path", http.path);
                params.set("host", http.host);
                break;
            case "quic":
                const quic = this.stream.quic;
                params.set("quicSecurity", quic.security);
                params.set("key", quic.key);
                params.set("headerType", quic.type);
                break;
            case "grpc":
                const grpc = this.stream.grpc;
                params.set("serviceName", grpc.serviceName);
                break;
        }

        if (this.stream.security === 'tls') {
            if (!ObjectUtil.isEmpty(this.stream.tls.server)) {
                address = this.stream.tls.server;
                params.set("sni", address);
            }
        }

        if (this.xtls) {
            params.set("flow", this.settings.vlesses[0].flow);
        }

        const link = `vless://${uuid}@${address}:${port}`;
        const url = new URL(link);
        for (const [key, value] of params) {
            url.searchParams.set(key, value)
        }
        url.hash = encodeURIComponent(remark);
        return url.toString();
    }

    genSSLink(address='', remark='') {
        let settings = this.settings;
        const server = this.stream.tls.server;
        if (!ObjectUtil.isEmpty(server)) {
            address = server;
        }
        return 'ss://' + safeBase64(settings.method + ':' + settings.password + '@' + address + ':' + this.port)
            + '#' + encodeURIComponent(remark);
    }

    genTrojanLink(address='', remark='') {
        let settings = this.settings;
        return `trojan://${settings.clients[0].password}@${address}:${this.port}#${encodeURIComponent(remark)}`;
    }

    genLink(address='', remark='') {
        switch (this.protocol) {
            case Protocols.VMESS: return this.genVmessLink(address, remark);
            case Protocols.VLESS: return this.genVLESSLink(address, remark);
            case Protocols.SHADOWSOCKS: return this.genSSLink(address, remark);
            case Protocols.TROJAN: return this.genTrojanLink(address, remark);
            default: return '';
        }
    }

    static fromJson(json={}) {
        return new Inbound(
            json.port,
            json.listen,
            json.protocol,
            Inbound.Settings.fromJson(json.protocol, json.settings),
            StreamSettings.fromJson(json.streamSettings),
            json.tag,
            Sniffing.fromJson(json.sniffing),
        )
    }

    toJson() {
        let streamSettings;
        if (this.canEnableStream() || this.protocol === Protocols.TROJAN) {
            streamSettings = this.stream.toJson();
        }
        return {
            port: this.port,
            listen: this.listen,
            protocol: this.protocol,
            settings: this.settings instanceof XrayCommonClass ? this.settings.toJson() : this.settings,
            streamSettings: streamSettings,
            tag: this.tag,
            sniffing: this.sniffing.toJson(),
        };
    }
}

Inbound.Settings = class extends XrayCommonClass {
    constructor(protocol) {
        super();
        this.protocol = protocol;
    }

    static getSettings(protocol) {
        switch (protocol) {
            case Protocols.VMESS: return new Inbound.VmessSettings(protocol);
            case Protocols.VLESS: return new Inbound.VLESSSettings(protocol);
            case Protocols.TROJAN: return new Inbound.TrojanSettings(protocol);
            case Protocols.SHADOWSOCKS: return new Inbound.ShadowsocksSettings(protocol);
            case Protocols.DOKODEMO: return new Inbound.DokodemoSettings(protocol);
            case Protocols.MTPROTO: return new Inbound.MtprotoSettings(protocol);
            case Protocols.SOCKS: return new Inbound.SocksSettings(protocol);
            case Protocols.HTTP: return new Inbound.HttpSettings(protocol);
            default: return null;
        }
    }

    static fromJson(protocol, json) {
        switch (protocol) {
            case Protocols.VMESS: return Inbound.VmessSettings.fromJson(json);
            case Protocols.VLESS: return Inbound.VLESSSettings.fromJson(json);
            case Protocols.TROJAN: return Inbound.TrojanSettings.fromJson(json);
            case Protocols.SHADOWSOCKS: return Inbound.ShadowsocksSettings.fromJson(json);
            case Protocols.DOKODEMO: return Inbound.DokodemoSettings.fromJson(json);
            case Protocols.MTPROTO: return Inbound.MtprotoSettings.fromJson(json);
            case Protocols.SOCKS: return Inbound.SocksSettings.fromJson(json);
            case Protocols.HTTP: return Inbound.HttpSettings.fromJson(json);
            default: return null;
        }
    }

    toJson() {
        return {};
    }
};

Inbound.VmessSettings = class extends Inbound.Settings {
    constructor(protocol,
                vmesses=[new Inbound.VmessSettings.Vmess()],
                disableInsecureEncryption=false) {
        super(protocol);
        this.vmesses = vmesses;
        this.disableInsecure = disableInsecureEncryption;
    }

    indexOfVmessById(id) {
        return this.vmesses.findIndex(vmess => vmess.id === id);
    }

    addVmess(vmess) {
        if (this.indexOfVmessById(vmess.id) >= 0) {
            return false;
        }
        this.vmesses.push(vmess);
    }

    delVmess(vmess) {
        const i = this.indexOfVmessById(vmess.id);
        if (i >= 0) {
            this.vmesses.splice(i, 1);
        }
    }

    static fromJson(json={}) {
        return new Inbound.VmessSettings(
            Protocols.VMESS,
            json.clients.map(client => Inbound.VmessSettings.Vmess.fromJson(client)),
            ObjectUtil.isEmpty(json.disableInsecureEncryption) ? false : json.disableInsecureEncryption,
        );
    }

    toJson() {
        return {
            clients: Inbound.VmessSettings.toJsonArray(this.vmesses),
            disableInsecureEncryption: this.disableInsecure,
        };
    }
};
Inbound.VmessSettings.Vmess = class extends XrayCommonClass {
    constructor(id=RandomUtil.randomUUID(), alterId=0) {
        super();
        this.id = id;
        this.alterId = alterId;
    }

    static fromJson(json={}) {
        return new Inbound.VmessSettings.Vmess(
            json.id,
            json.alterId,
        );
    }
};

Inbound.VLESSSettings = class extends Inbound.Settings {
    constructor(protocol,
                vlesses=[new Inbound.VLESSSettings.VLESS()],
                decryption='none',
                fallbacks=[],) {
        super(protocol);
        this.vlesses = vlesses;
        this.decryption = decryption;
        this.fallbacks = fallbacks;
    }

    addFallback() {
        this.fallbacks.push(new Inbound.VLESSSettings.Fallback());
    }

    delFallback(index) {
        this.fallbacks.splice(index, 1);
    }

    static fromJson(json={}) {
        return new Inbound.VLESSSettings(
            Protocols.VLESS,
            json.clients.map(client => Inbound.VLESSSettings.VLESS.fromJson(client)),
            json.decryption,
            Inbound.VLESSSettings.Fallback.fromJson(json.fallbacks),
        );
    }

    toJson() {
        return {
            clients: Inbound.VLESSSettings.toJsonArray(this.vlesses),
            decryption: this.decryption,
            fallbacks: Inbound.VLESSSettings.toJsonArray(this.fallbacks),
        };
    }
};
Inbound.VLESSSettings.VLESS = class extends XrayCommonClass {

    // 旧默认值 FLOW_CONTROL.DIRECT（xtls-rprx-direct）随 Step 2 一并移除；
    // 默认不带 flow，用户需要 Vision 时通过表单显式选择
    // （canEnableVision() 还要求外层是 tls/reality，新建入站默认两者都没开）。
    constructor(id=RandomUtil.randomUUID(), flow='') {
        super();
        this.id = id;
        this.flow = flow;
    }

    static fromJson(json={}) {
        return new Inbound.VLESSSettings.VLESS(
            json.id,
            json.flow,
        );
    }
};
Inbound.VLESSSettings.Fallback = class extends XrayCommonClass {
    constructor(name="", alpn='', path='', dest='', xver=0) {
        super();
        this.name = name;
        this.alpn = alpn;
        this.path = path;
        this.dest = dest;
        this.xver = xver;
    }

    toJson() {
        let xver = this.xver;
        if (!Number.isInteger(xver)) {
            xver = 0;
        }
        return {
            name: this.name,
            alpn: this.alpn,
            path: this.path,
            dest: this.dest,
            xver: xver,
        }
    }

    static fromJson(json=[]) {
        const fallbacks = [];
        for (let fallback of json) {
            fallbacks.push(new Inbound.VLESSSettings.Fallback(
                fallback.name,
                fallback.alpn,
                fallback.path,
                fallback.dest,
                fallback.xver,
            ))
        }
        return fallbacks;
    }
};

Inbound.TrojanSettings = class extends Inbound.Settings {
    constructor(protocol,
                clients=[new Inbound.TrojanSettings.Client()],
                fallbacks=[],) {
        super(protocol);
        this.clients = clients;
        this.fallbacks = fallbacks;
    }

    addTrojanFallback() {
        this.fallbacks.push(new Inbound.TrojanSettings.Fallback());
    }

    delTrojanFallback(index) {
        this.fallbacks.splice(index, 1);
    }

    toJson() {
        return {
            clients: Inbound.TrojanSettings.toJsonArray(this.clients),
            fallbacks: Inbound.TrojanSettings.toJsonArray(this.fallbacks),
        };
    }

    static fromJson(json={}) {
        const clients = [];
        for (const c of json.clients) {
            clients.push(Inbound.TrojanSettings.Client.fromJson(c));
        }
        return new Inbound.TrojanSettings(
            Protocols.TROJAN,
            clients,
            Inbound.TrojanSettings.Fallback.fromJson(json.fallbacks),);
    }
};
Inbound.TrojanSettings.Client = class extends XrayCommonClass {
    // 同上：旧默认值 FLOW_CONTROL.DIRECT 已随 Step 2 移除，默认不带 flow。
    constructor(password=RandomUtil.randomSeq(10), flow='') {
        super();
        this.password = password;
        this.flow = flow;
    }

    toJson() {
        return {
            password: this.password,
            flow: this.flow,
        };
    }

    static fromJson(json={}) {
        return new Inbound.TrojanSettings.Client(
            json.password,
            json.flow,
        );
    }

};

Inbound.TrojanSettings.Fallback = class extends XrayCommonClass {
    constructor(name="", alpn='', path='', dest='', xver=0) {
        super();
        this.name = name;
        this.alpn = alpn;
        this.path = path;
        this.dest = dest;
        this.xver = xver;
    }

    toJson() {
        let xver = this.xver;
        if (!Number.isInteger(xver)) {
            xver = 0;
        }
        return {
            name: this.name,
            alpn: this.alpn,
            path: this.path,
            dest: this.dest,
            xver: xver,
        }
    }

    static fromJson(json=[]) {
        const fallbacks = [];
        for (let fallback of json) {
            fallbacks.push(new Inbound.TrojanSettings.Fallback(
                fallback.name,
                fallback.alpn,
                fallback.path,
                fallback.dest,
                fallback.xver,
            ))
        }
        return fallbacks;
    }
};

Inbound.ShadowsocksSettings = class extends Inbound.Settings {
    constructor(protocol,
                method=SSMethods.AES_256_GCM,
                password=RandomUtil.randomSeq(10),
                network='tcp,udp'
    ) {
        super(protocol);
        this.method = method;
        this.password = password;
        this.network = network;
    }

    static fromJson(json={}) {
        return new Inbound.ShadowsocksSettings(
            Protocols.SHADOWSOCKS,
            json.method,
            json.password,
            json.network,
        );
    }

    toJson() {
        return {
            method: this.method,
            password: this.password,
            network: this.network,
        };
    }
};

Inbound.DokodemoSettings = class extends Inbound.Settings {
    constructor(protocol, address, port, network='tcp,udp') {
        super(protocol);
        this.address = address;
        this.port = port;
        this.network = network;
    }

    static fromJson(json={}) {
        return new Inbound.DokodemoSettings(
            Protocols.DOKODEMO,
            json.address,
            json.port,
            json.network,
        );
    }

    toJson() {
        return {
            address: this.address,
            port: this.port,
            network: this.network,
        };
    }
};

Inbound.MtprotoSettings = class extends Inbound.Settings {
    constructor(protocol, users=[new Inbound.MtprotoSettings.MtUser()]) {
        super(protocol);
        this.users = users;
    }

    static fromJson(json={}) {
        return new Inbound.MtprotoSettings(
            Protocols.MTPROTO,
            json.users.map(user => Inbound.MtprotoSettings.MtUser.fromJson(user)),
        );
    }

    toJson() {
        return {
            users: XrayCommonClass.toJsonArray(this.users),
        };
    }
};
Inbound.MtprotoSettings.MtUser = class extends XrayCommonClass {
    constructor(secret=RandomUtil.randomMTSecret()) {
        super();
        this.secret = secret;
    }

    static fromJson(json={}) {
        return new Inbound.MtprotoSettings.MtUser(json.secret);
    }
};

Inbound.SocksSettings = class extends Inbound.Settings {
    constructor(protocol, auth='password', accounts=[new Inbound.SocksSettings.SocksAccount()], udp=false, ip='127.0.0.1') {
        super(protocol);
        this.auth = auth;
        this.accounts = accounts;
        this.udp = udp;
        this.ip = ip;
    }

    addAccount(account) {
        this.accounts.push(account);
    }

    delAccount(index) {
        this.accounts.splice(index, 1);
    }

    static fromJson(json={}) {
        let accounts;
        if (json.auth === 'password') {
            accounts = json.accounts.map(
                account => Inbound.SocksSettings.SocksAccount.fromJson(account)
            )
        }
        return new Inbound.SocksSettings(
            Protocols.SOCKS,
            json.auth,
            accounts,
            json.udp,
            json.ip,
        );
    }

    toJson() {
        return {
            auth: this.auth,
            accounts: this.auth === 'password' ? this.accounts.map(account => account.toJson()) : undefined,
            udp: this.udp,
            ip: this.ip,
        };
    }
};
Inbound.SocksSettings.SocksAccount = class extends XrayCommonClass {
    constructor(user=RandomUtil.randomSeq(10), pass=RandomUtil.randomSeq(10)) {
        super();
        this.user = user;
        this.pass = pass;
    }

    static fromJson(json={}) {
        return new Inbound.SocksSettings.SocksAccount(json.user, json.pass);
    }
};

Inbound.HttpSettings = class extends Inbound.Settings {
    constructor(protocol, accounts=[new Inbound.HttpSettings.HttpAccount()]) {
        super(protocol);
        this.accounts = accounts;
    }

    addAccount(account) {
        this.accounts.push(account);
    }

    delAccount(index) {
        this.accounts.splice(index, 1);
    }

    static fromJson(json={}) {
        return new Inbound.HttpSettings(
            Protocols.HTTP,
            json.accounts.map(account => Inbound.HttpSettings.HttpAccount.fromJson(account)),
        );
    }

    toJson() {
        return {
            accounts: Inbound.HttpSettings.toJsonArray(this.accounts),
        };
    }
};

Inbound.HttpSettings.HttpAccount = class extends XrayCommonClass {
    constructor(user=RandomUtil.randomSeq(10), pass=RandomUtil.randomSeq(10)) {
        super();
        this.user = user;
        this.pass = pass;
    }

    static fromJson(json={}) {
        return new Inbound.HttpSettings.HttpAccount(json.user, json.pass);
    }
};
