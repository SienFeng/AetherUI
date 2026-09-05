// 侧边栏版本区的共享逻辑。
//
// common_sider.html 被 index / inbounds / routing / setting 四个页面共用，
// 但每个页面各有一个 new Vue({el:'#app'})，data 互不相干。版本区的指令写在
// sider 里，这些 data 就必须四个实例都有——少一个，那个页面会引用 undefined。
// 抄四遍将来必然漏改，所以做成 mixin。
//
// mixin 的 data 必须是函数（Vue 2 规则）；根实例的 data 是对象字面量，
// Vue 的 mergeDataOrFn 会正确合并两者，不用改现有页面的 data 写法。
const panelVersionMixin = {
    data() {
        return {
            panelVersion: {
                current: '',
                latest: '',
                hasUpdate: false,
                knownCurrent: false,
                releases: [],
                checkedAt: 0,
                lastError: '',
                updatable: false,
                unsupportedReason: '',
            },
            versionPopoverVisible: false,
            versionRefreshing: false,
            rollbackOpen: false,
            rollbackTag: '',
            // idle | starting | restarting | done | timeout
            upgradeState: 'idle',
            upgradeTarget: '',
            // 一旦观察到过一次「连不上 = 正在重启」，就不再退回 starting——
            // 见 pollUpgrade 里的说明。
            upgradeSawRestart: false,
            upgradeLogVisible: false,
            upgradeLogLines: [],
        };
    },
    methods: {
        async loadPanelVersion() {
            const msg = await HttpUtil.post('/server/panelVersion');
            if (msg.success && msg.obj) {
                this.panelVersion = msg.obj;
            }
        },
        async refreshPanelVersion() {
            this.versionRefreshing = true;
            const msg = await HttpUtil.post('/server/refreshPanelVersion');
            this.versionRefreshing = false;
            if (msg.success && msg.obj) {
                this.panelVersion = msg.obj;
            }
        },
        formatReleaseDate(ms) {
            if (!ms) return '';
            return moment(ms).format('YYYY/M/D');
        },
        // 版本区的状态文案。三种状态必须能区分开：
        // 「已是最新」「有新版」「当前版本不在发布列表里」。
        versionStateText() {
            const v = this.panelVersion;
            if (!v.checkedAt) return v.lastError ? '检查失败' : '尚未检查';
            if (!v.knownCurrent) return '未在发布列表中';
            return v.hasUpdate ? ('有新版本 ' + v.latest) : '已是最新版本';
        },
        confirmUpgrade(tag, isRollback) {
            // 从回退列表选到的如果恰好是最新版，那是「更新」不是「回退」——
            // 当前版本不在发布列表里时（本地构建 / 落后太多），回退列表是管理员
            // 唯一能回到正式版的入口，此时说「新功能会失效」是反的。
            const goingBack = isRollback && tag !== this.panelVersion.latest;
            const content = goingBack
                ? ('确定要回退到 ' + tag + ' 吗？\n\n'
                    + '回退会一并把 xray 核心换成该版本携带的构建，新版新增的功能会失效。'
                    + '数据库和已有配置不会丢失。')
                : ('确定要更新到 ' + tag + ' 吗？\n\n面板会在更新过程中短暂不可用。');
            this.$confirm({
                title: goingBack ? '回退到旧版本' : '更新面板',
                content: h => h('div', content.split('\n').map(line => h('p', line))),
                okText: '确定',
                cancelText: '取消',
                onOk: () => this.startUpgrade(tag),
            });
        },
        async startUpgrade(tag) {
            this.upgradeTarget = tag;
            this.upgradeSawRestart = false;
            this.upgradeState = 'starting';
            const msg = await HttpUtil.post('/server/upgradePanel', { version: tag });
            if (!msg.success) {
                this.upgradeState = 'idle';
                return;
            }
            this.pollUpgrade(tag, Date.now());
        },
        // 轮询直到 current 变成目标版本。面板会在中途被 stop，请求会失败，
        // 那正是「正在重启」的信号，要继续重试而不是报错退出。
        //
        // 刻意不用 HttpUtil.post：它内部自带 try/catch，网络失败时只会返回
        // 一个 success=false 的 Msg（不抛异常），并且每次失败都会弹一次红色
        // $message.error——面板重启的这几十秒里请求失败是预期内的信号，不是
        // 需要打扰管理员的错误，用 axios.post 自己解析结果才能既捕获这个
        // “连不上”状态，又不产生噪音。
        async pollUpgrade(tag, startedAt) {
            const TIMEOUT_MS = 3 * 60 * 1000;
            if (Date.now() - startedAt > TIMEOUT_MS) {
                this.upgradeState = 'timeout';
                return;
            }
            await PromiseUtil.sleep(3000);
            let reached = false;
            try {
                const resp = await axios.post('/server/panelVersion');
                const body = resp && resp.data;
                if (body && body.success === false) {
                    // 会话过期：checkLogin 对 ajax 返回 200 + success:false，
                    // axios 不会抛异常。不区分的话会一路轮询到超时，
                    // 报一个「更新可能失败」——而更新其实可能已经成功了。
                    this.upgradeState = 'timeout';
                    this.upgradeTarget = tag;
                    return;
                }
                const obj = body && body.obj;
                if (obj) {
                    this.panelVersion = obj;
                    reached = obj.current === tag;
                }
                // 一次成功但未达标的轮询不该把 'restarting' 打回 'starting'——
                // 面板重启期间偶尔能立即答上一次请求（旧进程还没退出）很正常，
                // 不代表重启没有发生，来回切换文案只会让人以为卡住了。一旦观察
                // 到过重启，就保持 'restarting' 直到达标或超时。
                if (!reached && !this.upgradeSawRestart) this.upgradeState = 'starting';
            } catch (e) {
                // 连不上 = 面板正在重启，这是预期内的
                this.upgradeSawRestart = true;
                this.upgradeState = 'restarting';
            }
            if (reached) {
                this.upgradeState = 'done';
                return;
            }
            this.pollUpgrade(tag, startedAt);
        },
        async openUpgradeLog() {
            const msg = await HttpUtil.post('/server/upgradeLog');
            if (msg.success && msg.obj) {
                this.upgradeLogLines = msg.obj.lines || [];
                this.upgradeLogVisible = true;
            }
        },
    },
    mounted() {
        this.loadPanelVersion();
    },
};
