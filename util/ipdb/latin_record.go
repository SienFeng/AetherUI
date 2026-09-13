package ipdb

// LatinRecord 按拉丁字母数据源的字段构造一条 Record。
//
// IP2Location LITE DB3 与 DB-IP City Lite 的字段形状完全一致（国家码 + 省 + 市），
// 归一逻辑因此收在这一处：两个解析器各自实现会漂移，而漂移之后两个源对同一个
// 地方给出的写法不同，多源比较全部失效，界面上却只表现为「这两个源老是不一致」。
//
// 第二个返回值为 false 表示国家码不认识，调用方应当整条丢弃。不留一条空国家的
// 记录：那种记录既不算中国也没有国家，在界面上是一片空白，也无从判断是查不到
// 还是没收录。
//
// 与 normalize 保持同一套精度取舍：境外段只保留国家。两个源都没有 ISP 字段，
// 所以境外段那个「知名 IDC / 云厂商」的位置恒为空——这是数据本身的缺口，不是
// 归一丢掉的，共享检测的画像键会因此退化成带网络族的降级形态（见
// selectNetworkMeta 的注释）。
func LatinRecord(start, end uint32, countryCode, region, city string) (Record, bool) {
	country, ok := CountryFromCode(countryCode)
	if !ok {
		return Record{}, false
	}
	rec := Record{Start: start, End: end, Country: country}
	if country != ChinaCountry {
		return rec, true
	}
	// 省份认不出就只留国家。硬塞一个原文会让同一个省因为写法不同被当成两个
	// 地区，多源合并与地区限制同时失效。
	province, ok := ProvinceFromLatin(region)
	if !ok {
		return rec, true
	}
	rec.Region = province
	// 城市翻译不出来就留空，绝不原样存拉丁名，理由见 CityFromLatin。
	if name, ok := CityFromLatin(province, city); ok {
		rec.City = name
	}
	return rec, true
}
