const RULE_ACTION = {
    PROXY: "proxy",
    BLOCK: "block",
};

const ACTION_LABEL = {
    proxy: "走节点",
    block: "阻断",
};

// 列表接口返回的是摘要：域名组挂上订阅后可能有几万条域名，
// 全量传给前端既没意义，渲染几万个 tag 还会卡死浏览器。
// 编辑弹窗需要的手工域名原文由 detail 接口单独取。
class DomainGroup {
    constructor(json = {}) {
        this.id = json.id || 0;
        this.remark = json.remark || "";
        this.preview = json.preview || [];
        this.effectiveCount = json.effectiveCount || 0;
        this.manualCount = json.manualCount || 0;
        this.subscribedCount = json.subscribedCount || 0;
        this.subscribeUrl = json.subscribeUrl || "";
        this.lastUpdatedAt = json.lastUpdatedAt || 0;
        this.lastError = json.lastError || "";
        this.lastSkipped = json.lastSkipped || 0;
        // Domains 或 SubscribedDomains 任一列 JSON 解码失败：buildRule 会因
        // 「域名组数据损坏」整条丢弃引用它的规则，effectiveCount 已被后端强制
        // 置 0，这里单独留一个标记方便渲染出与「本来就是空」不同的提示。
        this.broken = json.broken || false;
    }

    static fromJson(json = {}) {
        return new DomainGroup(json);
    }

    get subscribed() {
        return this.subscribeUrl !== "";
    }

    // 订阅状态：未订阅 / 失败 / 等待首次拉取 / 成功
    get subscribeState() {
        if (!this.subscribed) return "none";
        if (this.lastError) return "error";
        if (!this.lastUpdatedAt) return "pending";
        return "ok";
    }
}

class OutboundNode {
    constructor(id = 0, tag = "", remark = "", protocol = "", config = "", enable = true) {
        this.id = id;
        this.tag = tag;
        this.remark = remark;
        this.protocol = protocol;
        this.config = config;
        this.enable = enable;
    }

    static fromJson(json = {}) {
        return new OutboundNode(json.id, json.tag, json.remark, json.protocol, json.config, json.enable);
    }
}

class RoutingRule {
    constructor(id = 0, remark = "", inboundIds = [], domainGroupId = 0,
                action = RULE_ACTION.PROXY, outboundId = 0, priority = 0,
                enable = true, broken = false) {
        this.id = id;
        this.remark = remark;
        // 空数组 = 所有用户（含以后新建的入站）。
        // 注意它与「一个用户都没勾」在提交体里长得一模一样，弹窗必须自己
        // 区分这两种意图，见 routing.html 的 saveRule。
        this.inboundIds = inboundIds;
        this.domainGroupId = domainGroupId;
        this.action = action;
        this.outboundId = outboundId;
        this.priority = priority;
        this.enable = enable;
        // broken 为真表示服务端解码 inboundIds 失败。这种规则不会写进配置，
        // 但它的 inboundIds 是空数组，看起来和「所有用户」一样，必须区分渲染。
        this.broken = broken;
    }

    static fromJson(json = {}) {
        return new RoutingRule(json.id, json.remark, json.inboundIds || [],
            json.domainGroupId, json.action, json.outboundId, json.priority,
            json.enable, json.broken);
    }
}
