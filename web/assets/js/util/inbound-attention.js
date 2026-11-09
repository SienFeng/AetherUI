// 入站「需要留意」摘要的共享拉取逻辑：侧栏「入站列表」的提醒点 + 入站列表里的停用原因、
// 自动恢复时刻与地区拦截。
//
// 与 routing-audit.js 同理：common_sider.html 被四个页面共用，提醒点的指令写在 sider 里，
// 每个页面的 Vue 实例都必须有 inboundAttention，所以做成 mixin。
//
// /aui/inbound/attention/summary 只读库、不写任何东西。到期与地区拦截变化慢，5 分钟一轮；
// 页面隐藏时跳过，重新可见时若距上次超过一个周期就立即补一次。入站列表在操作后会自己补拉。
const INBOUND_ATTENTION_INTERVAL_MS = 5 * 60 * 1000;

const inboundAttentionMixin = {
    data() {
        return {
            inboundAttention: {
                // { "<入站 id>": { disabledReason, autoRestoreAt, expiresSoon, geoBlocked } }
                byId: {},
                // 侧栏提醒点：7 天内到期、近 24 小时被地区限制拦截、或地区规则降级的启用入站数
                alertCount: 0,
                loadedAt: 0,
            },
            inboundAttentionTimer: null,
            inboundAttentionSeq: 0,
            inboundAttentionAppliedSeq: 0,
        };
    },
    methods: {
        // 不用 HttpUtil.post：后台轮询失败是常态（网络抖动、面板重启），每轮弹一条错误只是噪音。
        // 失败时保留上一次的摘要，不清空——清空会让提醒凭空消失。
        async refreshInboundAttention() {
            const seq = ++this.inboundAttentionSeq;
            let body;
            try {
                const resp = await axios.post('aui/inbound/attention/summary');
                body = resp && resp.data;
            } catch (e) {
                body = null;
            }
            if (seq <= this.inboundAttentionAppliedSeq) return;
            if (!body || body.success !== true) return;
            this.inboundAttentionAppliedSeq = seq;
            const byId = body.obj || {};
            Object.assign(this.inboundAttention, {
                byId,
                alertCount: Object.keys(byId).filter(id => byId[id] && (byId[id].expiresSoon || byId[id].geoBlocked || byId[id].geoDegraded)).length,
                loadedAt: Date.now(),
            });
        },
        inboundAttentionOf(id) {
            return this.inboundAttention.byId[id] || {};
        },
        inboundAttentionTick() {
            if (document.hidden) return;
            this.refreshInboundAttention();
        },
        onInboundAttentionVisibility() {
            if (document.hidden) return;
            if (Date.now() - this.inboundAttention.loadedAt >= INBOUND_ATTENTION_INTERVAL_MS) this.refreshInboundAttention();
        },
    },
    mounted() {
        this.refreshInboundAttention();
        this.inboundAttentionTimer = setInterval(() => this.inboundAttentionTick(), INBOUND_ATTENTION_INTERVAL_MS);
        document.addEventListener('visibilitychange', this.onInboundAttentionVisibility);
    },
    beforeDestroy() {
        if (this.inboundAttentionTimer) clearInterval(this.inboundAttentionTimer);
        this.inboundAttentionTimer = null;
        document.removeEventListener('visibilitychange', this.onInboundAttentionVisibility);
    },
};
