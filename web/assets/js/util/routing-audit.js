// 分流规则后台审计摘要的共享拉取逻辑（侧栏红点 + 分流页规则列表的状态图标）。
//
// 与 panel-version.js 同理：common_sider.html 被四个页面共用，红点的指令写在
// sider 里，四个 Vue 实例都必须有 routingAudit，所以做成 mixin。
//
// /aui/routing/rule/audit 只读内存里的缓存与异常摘要，不触发验证——轮询它
// 很便宜；这里也绝不能顺手去调 verify。页面隐藏（document.hidden）时跳过
// 这一轮，重新可见时若距上次超过一个周期就立即补一次。
const ROUTING_AUDIT_INTERVAL_MS = 60 * 1000;
// 最近一次成功拉取早于这么久，摘要就不再代表现状（两个周期：允许偶尔失败一轮，
// 连续两轮失败——约 2 分钟没刷新——即改灰）。判定在每轮拉取发起/失败时更新，
// 所以阈值必须 ≤ 两个周期，取 150 秒的话要到第三轮（约 3 分钟）才会触发。
const ROUTING_AUDIT_STALE_MS = 120 * 1000;

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
                // 最近一次发起拉取 / 最近一次失败的时刻。每一轮都会更新（失败也是），
                // 「摘要过时了」由它与 loadedAt 之差判断，所以是响应式的，不依赖 Date.now()。
                lastAttemptAt: 0,
                failedAt: 0,
                accessLogEnabled: false,
                // 非空表示服务端读不出访问日志开关——不等于「未开启」，要分开显示
                accessLogError: '',
            },
            routingAuditTimer: null,
            // 请求序号：定时、页面重新可见、操作后补拉可能重叠，晚到的旧响应不能盖掉新的
            routingAuditSeq: 0,
            routingAuditAppliedSeq: 0,
        };
    },
    methods: {
        // 刻意不用 HttpUtil.post：它每次失败都会弹一条红色 $message.error，
        // 后台轮询在网络抖动、面板重启期间失败是常态，每分钟弹一次只是噪音。
        // 失败时保留上一次的摘要（不清空——清空会让红点凭空消失），只记下失败时刻；
        // 过时太久的摘要由 routingAuditStale() 让界面改口，不会一直显示旧的绿色。
        // axios 仍带着 axios-init.js 的 X-Requested-With，过得了 csrfGuard。
        async refreshRoutingAudit() {
            const seq = ++this.routingAuditSeq;
            this.routingAudit.lastAttemptAt = Date.now();
            let body;
            try {
                const resp = await axios.post('aui/routing/rule/audit');
                body = resp && resp.data;
            } catch (e) {
                body = null;
            }
            if (seq <= this.routingAuditAppliedSeq) return;
            // 会话过期时 checkLogin 返回登录页 HTML 或 success:false，同样算失败
            if (!body || body.success !== true || !body.obj) {
                this.routingAudit.failedAt = Date.now();
                return;
            }
            this.routingAuditAppliedSeq = seq;
            const obj = body.obj;
            const rules = obj.rules || {};
            Object.assign(this.routingAudit, {
                applyGen: obj.applyGen || 0,
                rules,
                mismatchCount: Object.keys(rules).filter(id => rules[id] && rules[id].verdict === 'mismatch').length,
                loadedAt: Date.now(),
                accessLogEnabled: !!obj.accessLogEnabled,
                accessLogError: obj.accessLogError || '',
            });
        },
        // 拿到过摘要，但最近一次成功已经太久（之后的拉取都失败或还没回来）
        routingAuditStale() {
            const a = this.routingAudit;
            if (!a.loadedAt) return false;
            return Math.max(a.lastAttemptAt, a.failedAt) - a.loadedAt >= ROUTING_AUDIT_STALE_MS;
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
