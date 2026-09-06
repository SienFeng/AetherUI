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
        this.cidrCount = json.cidrCount || 0;
        this.subscribedCidrCount = json.subscribedCidrCount || 0;
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
    constructor(id = 0, remark = "", inboundIds = [], domainGroupIds = [],
                action = RULE_ACTION.PROXY, outboundId = 0, priority = 0,
                enable = true, broken = false, groupsBroken = false) {
        this.id = id;
        this.remark = remark;
        // 空数组 = 所有用户（含以后新建的入站）。
        // 注意它与「一个用户都没勾」在提交体里长得一模一样，弹窗必须自己
        // 区分这两种意图，见 routing.html 的 saveRule。
        this.inboundIds = inboundIds;
        // 与 inboundIds 相反：空数组【不是】「所有域名组」，而是非法状态。
        // 域名条件为空会让 xray 把规则当作「不限制」，规则从「这批域名走 B」
        // 退化成「该用户全部流量走 B」。saveRule 必须自己拦住它。
        this.domainGroupIds = domainGroupIds;
        this.action = action;
        this.outboundId = outboundId;
        this.priority = priority;
        this.enable = enable;
        // broken 为真表示服务端解码 inboundIds 失败。这种规则不会写进配置，
        // 但它的 inboundIds 是空数组，看起来和「所有用户」一样，必须区分渲染。
        this.broken = broken;
        // groupsBroken 为真表示服务端解码 domainGroupIds 失败。与 broken 分开
        // 是因为界面文案不同，合并会让管理员照着去修错的地方。
        this.groupsBroken = groupsBroken;
    }

    static fromJson(json = {}) {
        return new RoutingRule(json.id, json.remark, json.inboundIds || [],
            json.domainGroupIds || [], json.action, json.outboundId, json.priority,
            json.enable, json.broken, json.groupsBroken);
    }
}
