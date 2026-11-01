// 分流规则后台审计摘要的共享拉取逻辑（侧栏红点 + 分流页规则列表的状态图标）。
//
// 与 panel-version.js 同理：common_sider.html 被四个页面共用，红点的指令写在
// sider 里，四个 Vue 实例都必须有 routingAudit，所以做成 mixin。
//
// /aui/routing/rule/audit 只读内存里的缓存与异常摘要，不触发验证——轮询它
// 很便宜；这里也绝不能顺手去调 verify。页面隐藏（document.hidden）时跳过
// 这一轮，重新可见时若距上次超过一个周期就立即补一次。
const ROUTING_AUDIT_INTERVAL_MS = 60 * 1000;

const routingAuditMixin = {
    data() {
        return {
            routingAudit: {
                applyGen: 0,
                // { "<规则 id>": { verdict, checkedAt, applyGen, stale, mismatchUsers, anomalyCount, anomalyLastAt } }
                rules: {},
                mismatchCount: 0,
                // 最近一次成功拉取的时刻（毫秒）；0 表示还没拿到过，界面据此不显示状态
                loadedAt: 0,
                accessLogEnabled: false,
                // 非空表示服务端读不出访问日志开关——不等于「未开启」，要分开显示
                accessLogError: '',
            },
            routingAuditTimer: null,
        };
    },
    methods: {
        // 刻意不用 HttpUtil.post：它每次失败都会弹一条红色 $message.error，
        // 后台轮询在网络抖动、面板重启期间失败是常态，每分钟弹一次只是噪音。
        // 失败时保留上一次的摘要（不清空——清空会让红点凭空消失），等下一轮。
        // axios 仍带着 axios-init.js 的 X-Requested-With，过得了 csrfGuard。
        async refreshRoutingAudit() {
            let body;
            try {
                const resp = await axios.post('aui/routing/rule/audit');
                body = resp && resp.data;
            } catch (e) {
                return;
            }
            if (!body || !body.success || !body.obj) return;
            const obj = body.obj;
            const rules = obj.rules || {};
            this.routingAudit = {
                applyGen: obj.applyGen || 0,
                rules,
                mismatchCount: Object.keys(rules).filter(id => rules[id] && rules[id].verdict === 'mismatch').length,
                loadedAt: Date.now(),
                accessLogEnabled: !!obj.accessLogEnabled,
                accessLogError: obj.accessLogError || '',
            };
        },
        routingAuditTick() {
            if (document.hidden) return;
            this.refreshRoutingAudit();
        },
        onRoutingAuditVisibility() {
            if (document.hidden) return;
            if (Date.now() - this.routingAudit.loadedAt >= ROUTING_AUDIT_INTERVAL_MS) this.refreshRoutingAudit();
        },
    },
    mounted() {
        this.refreshRoutingAudit();
        this.routingAuditTimer = setInterval(() => this.routingAuditTick(), ROUTING_AUDIT_INTERVAL_MS);
        document.addEventListener('visibilitychange', this.onRoutingAuditVisibility);
    },
    beforeDestroy() {
        if (this.routingAuditTimer) clearInterval(this.routingAuditTimer);
        this.routingAuditTimer = null;
        document.removeEventListener('visibilitychange', this.onRoutingAuditVisibility);
    },
};
