// 从前端模型（web/assets/js/model/xray.js）生成 Go 侧 golden 测试的期望 JSON。
//
// Go 侧手写的入站 JSON 必须与前端模型逐字段一致：字段名差一个字母 xray 照样能跑，
// 但管理员用面板打开这个入站时表单会错乱或吞值——xray 的配置校验看不见这类差异。
// golden 由本脚本生成而非手写，保证它确实是前端的真实输出。
//
// 重新生成：node bootstrap/testdata/gen_golden.js > bootstrap/testdata/reality_inbound.golden.json
const fs = require("fs");
const vm = require("vm");
const path = require("path");

const repo = path.resolve(__dirname, "../..");
const src =
  fs.readFileSync(path.join(repo, "web/assets/js/util/utils.js"), "utf8") + "\n" +
  fs.readFileSync(path.join(repo, "web/assets/js/model/xray.js"), "utf8") + `
const inbound = new Inbound(443, "", Protocols.VLESS);
inbound.settings.vlesses[0].id = "11111111-2222-3333-4444-555555555555";
inbound.settings.vlesses[0].flow = "xtls-rprx-vision";
inbound.stream.network = "tcp";
inbound.stream.security = "reality";
inbound.stream.reality.target = "www.tesla.com:443";
inbound.stream.reality.serverNames = "www.tesla.com";
inbound.stream.reality.privateKey = "aGVsbG8td29ybGQtdGVzdC1wcml2YXRlLWtleTEyMw";
inbound.stream.reality.shortIds = "0123456789abcdef";
inbound.stream.reality.settings.publicKey = "aGVsbG8td29ybGQtdGVzdC1wdWJsaWMta2V5MTIzNDU";
emit({
  settings: inbound.settings.toString(false),
  streamSettings: inbound.stream.toString(false),
  sniffing: inbound.sniffing.toString(false),
});
`;

const ctx = { console, emit: (o) => process.stdout.write(JSON.stringify(o, null, 2) + "\n") };
vm.createContext(ctx);
vm.runInContext(src, ctx, { filename: "gen_golden.js" });
