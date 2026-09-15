const oneMinute = 1000 * 60; // 一分钟的毫秒数
const oneHour = oneMinute * 60; // 一小时的毫秒数
const oneDay = oneHour * 24; // 一天的毫秒数
const oneWeek = oneDay * 7; // 一星期的毫秒数
const oneMonth = oneDay * 30; // 一个月的毫秒数

/**
 * 按天数减少
 *
 * @param days 要减少的天数
 */
Date.prototype.minusDays = function (days) {
    return this.minusMillis(oneDay * days);
};

/**
 * 按天数增加
 *
 * @param days 要增加的天数
 */
Date.prototype.plusDays = function (days) {
    return this.plusMillis(oneDay * days);
};

/**
 * 按小时减少
 *
 * @param hours 要减少的小时数
 */
Date.prototype.minusHours = function (hours) {
    return this.minusMillis(oneHour * hours);
};

/**
 * 按小时增加
 *
 * @param hours 要增加的小时数
 */
Date.prototype.plusHours = function (hours) {
    return this.plusMillis(oneHour * hours);
};

/**
 * 按分钟减少
 *
 * @param minutes 要减少的分钟数
 */
Date.prototype.minusMinutes = function (minutes) {
    return this.minusMillis(oneMinute * minutes);
};

/**
 * 按分钟增加
 *
 * @param minutes 要增加的分钟数
 */
Date.prototype.plusMinutes = function (minutes) {
    return this.plusMillis(oneMinute * minutes);
};

/**
 * 按毫秒减少
 *
 * @param millis 要减少的毫秒数
 */
Date.prototype.minusMillis = function(millis) {
    let time = this.getTime() - millis;
    let newDate = new Date();
    newDate.setTime(time);
    return newDate;
};

/**
 * 按毫秒增加
 *
 * @param millis 要增加的毫秒数
 */
Date.prototype.plusMillis = function(millis) {
    let time = this.getTime() + millis;
    let newDate = new Date();
    newDate.setTime(time);
    return newDate;
};

/**
 * 设置时间为当天的 00:00:00.000
 */
Date.prototype.setMinTime = function () {
    this.setHours(0);
    this.setMinutes(0);
    this.setSeconds(0);
    this.setMilliseconds(0);
    return this;
};

/**
 * 设置时间为当天的 23:59:59.999
 */
Date.prototype.setMaxTime = function () {
    this.setHours(23);
    this.setMinutes(59);
    this.setSeconds(59);
    this.setMilliseconds(999);
    return this;
};

/**
 * 格式化日期
 */
Date.prototype.formatDate = function () {
    return this.getFullYear() + "-" + addZero(this.getMonth() + 1) + "-" + addZero(this.getDate());
};

/**
 * 格式化时间
 */
Date.prototype.formatTime = function () {
    return addZero(this.getHours()) + ":" + addZero(this.getMinutes()) + ":" + addZero(this.getSeconds());
};

/**
 * 格式化日期加时间
 *
 * @param split 日期和时间之间的分隔符，默认是一个空格
 */
Date.prototype.formatDateTime = function (split = ' ') {
    return this.formatDate() + split + this.formatTime();
};

class DateUtil {

    // 字符串转 Date 对象
    static parseDate(str) {
        return new Date(str.replace(/-/g, '/'));
    }

    static formatMillis(millis) {
        const p = DateUtil.panelParts(millis);
        if (!p) {
            return moment(millis).format('YYYY-M-D H:m:s');
        }
        return `${p.year}-${p.month}-${p.day} ${p.hour}:${p.minute}:${p.second}`;
    }

    // 只要日期的场合（侧栏的版本发布日期）。
    static formatMillisDate(millis) {
        const p = DateUtil.panelParts(millis);
        if (!p) {
            return moment(millis).format('YYYY/M/D');
        }
        return `${p.year}/${p.month}/${p.day}`;
    }

    // 给人看的时间一律按面板时区，不按浏览器所在机器的时区。
    //
    // 服务端的用量曲线刻度、「今日」这类窗口、定时任务都按面板时区算，而
    // moment(ms) 用的是浏览器本地时区。管理员的电脑时区一旦与面板不同，同一
    // 页上表格与曲线就会差出整数个小时，而且哪边都没有标明自己是哪个时区。
    //
    // panelTimeZone 由服务端注入（web/controller/util.go 的 panelTimeZone），
    // 在 common/js.html 里先于本文件定义。浏览器不认识的值（设置里填 Local
    // 能通过服务端的 time.LoadLocation，Intl 却会抛 RangeError）与读库失败时
    // 注入的空串，都回落到浏览器本地时区——那是改动前的行为，时间只是换了个
    // 时区显示，不至于整个出错。
    static panelFormatter() {
        if (DateUtil.cachedPanelFormatter !== undefined) {
            return DateUtil.cachedPanelFormatter;
        }
        let formatter = null;
        if (typeof panelTimeZone === 'string' && panelTimeZone !== '') {
            try {
                formatter = new Intl.DateTimeFormat('en-US', {
                    timeZone: panelTimeZone,
                    // 不用 hour12: false：部分 Chrome 版本在它下面把零点输出成 24。
                    hourCycle: 'h23',
                    year: 'numeric', month: 'numeric', day: 'numeric',
                    hour: 'numeric', minute: 'numeric', second: 'numeric',
                });
            } catch (e) {
                formatter = null;
            }
        }
        DateUtil.cachedPanelFormatter = formatter;
        return formatter;
    }

    // 面板时区下 millis 这一刻的钟面时间，各字段是数字、月份从 1 开始。
    // 面板时区不可用时返回 null，由调用方回落到浏览器本地时区。
    static panelParts(millis) {
        const formatter = DateUtil.panelFormatter();
        if (!formatter) {
            return null;
        }
        const parts = {};
        formatter.formatToParts(new Date(millis)).forEach(p => { parts[p.type] = p.value; });
        return {
            year: Number(parts.year),
            month: Number(parts.month),
            day: Number(parts.day),
            hour: Number(parts.hour) % 24,
            minute: Number(parts.minute),
            second: Number(parts.second),
        };
    }

    // antd 的日期选择器只会按浏览器本地时区显示 moment。交给它一个「本地字段
    // 恰好等于面板时区钟面」的 moment，选择器上看到的就是面板时区的时间。
    // 选完之后必须经 fromPanelMoment 换算回毫秒，不能直接 valueOf()。
    static toPanelMoment(millis) {
        const p = DateUtil.panelParts(millis);
        if (!p) {
            return moment(millis);
        }
        return moment([p.year, p.month - 1, p.day, p.hour, p.minute, p.second]);
    }

    // toPanelMoment 的逆：把选择器里的钟面时间当成面板时区的时间，换算回毫秒。
    //
    // 偏移要取「结果那一刻」的，而结果正是要算的东西：先用钟面本身估一次，
    // 再用估出的时刻校正一次——跨夏令时切换时两次取到的偏移不同。面板时区里
    // 不存在的钟面时间（夏令时拨快跳过的那一小时）会落到相邻的有效时刻，不报错。
    static fromPanelMoment(m) {
        if (!DateUtil.panelFormatter()) {
            return m.valueOf();
        }
        const wall = Date.UTC(m.year(), m.month(), m.date(), m.hours(), m.minutes(), m.seconds(), m.milliseconds());
        const guess = wall - DateUtil.panelOffset(wall);
        return wall - DateUtil.panelOffset(guess);
    }

    // 面板时区在 millis 这一刻相对 UTC 的偏移（毫秒），东八区为 +8 小时。
    static panelOffset(millis) {
        const p = DateUtil.panelParts(millis);
        const wall = Date.UTC(p.year, p.month - 1, p.day, p.hour, p.minute, p.second);
        return wall - Math.floor(millis / 1000) * 1000;
    }

    static firstDayOfMonth() {
        const date = new Date();
        date.setDate(1);
        date.setMinTime();
        return date;
    }
}