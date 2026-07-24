package seed

// Curated bilingual catalog vocabulary. Names are intentionally bilingual
// (English + Simplified Chinese) to exercise realistic catalog copy. Brands,
// categories, adjectives, nouns, and option dimensions are all hand-picked so
// the golden dev summary is stable across generator revisions.

var catalogBrands = []string{
	"Northwind 北风",
	"Atlas 山海",
	"Cedar 雪松",
	"Harbor 港湾",
	"Lumen 流明",
	"Meridian 子午",
	"Pinegrove 松林",
	"Quartz 石英",
	"Riverside 河畔",
	"Saffron 藏红花",
	"Tundra 苔原",
	"Umber 赭石",
	"Verdant 翠绿",
	"Willow 垂柳",
	"Zephyr 西风",
	"Aurora 极光",
	"Boreal 北境",
	"Cobalt 钴蓝",
	"Driftwood 浮木",
	"Ember 余烬",
	"Fjord 峡湾",
	"Garnet 石榴石",
	"Halcyon 翠鸟",
	"Indigo 靛蓝",
	"Juniper 杜松",
	"Kestrel 红隼",
	"Lighthouse 灯塔",
	"Magnolia 木兰",
	"Nimbus 雨云",
	"Orchid 兰花",
	"Pebble 鹅卵石",
	"Quill 羽笔",
	"Rosewood 花梨",
	"Slate 板岩",
	"Tamarind 罗望子",
	"Umbel 伞形",
	"Vellum 羊皮纸",
	"Whisker 须",
	"Yew 紫杉",
	"Zenith 天顶",
}

var catalogCategoryRoots = []struct {
	ID   string
	Name string
	Path string
}{
	{"apparel", "Apparel 服饰", "/apparel"},
	{"electronics", "Electronics 电子", "/electronics"},
	{"home", "Home & Living 家居", "/home"},
	{"kitchen", "Kitchen 厨房", "/kitchen"},
	{"outdoor", "Outdoor 户外", "/outdoor"},
	{"beauty", "Beauty 美妆", "/beauty"},
	{"books", "Books 图书", "/books"},
	{"toys", "Toys 玩具", "/toys"},
	{"sports", "Sports 运动", "/sports"},
	{"grocery", "Grocery 食品", "/grocery"},
	{"stationery", "Stationery 文具", "/stationery"},
	{"garden", "Garden 花园", "/garden"},
}

// categoryLeaves pre-binds a leaf category to each root. The dev scale uses the
// first 8 roots (16 categories total: 8 roots + 8 leaves), matching the spec's
// exact 8-category count after pruning to a single curated level.
var categoryLeaves = []struct {
	Suffix string
	Name   string
}{
	{"-tops", "Tops 上装"},
	{"-accessories", "Accessories 配件"},
	{"-cookware", "Cookware 炊具"},
	{"-bedding", "Bedding 床品"},
	{"-tools", "Tools 工具"},
	{"-skincare", "Skincare 护肤"},
	{"-hardcover", "Hardcover 精装"},
	{"-figures", "Figures 手办"},
	{"-fitness", "Fitness 健身"},
	{"-pantry", "Pantry 储藏"},
	{"-pens", "Pens 笔"},
	{"-planters", "Planters 花盆"},
}

var productAdjectives = []string{
	"Everyday 日常", "Premium 臻选", "Classic 经典", "Modern 现代", "Compact 紧凑",
	"Deluxe 豪华", "Essential 必备", "Signature 招牌", "Heritage 传承", "Studio 工作室",
	"Artisan 手工", "Pro 专业", "Lite 轻量", "Max 旗舰", "Eco 环保",
}

var productNouns = []string{
	"Cotton Tee 棉质T恤", "Wool Scarf 羊毛围巾", "Bluetooth Speaker 蓝牙音箱",
	"USB-C Cable 数据线", "Ceramic Mug 陶瓷杯", "Cast Iron Pan 铸铁锅",
	"Linen Sheet 亚麻床单", "Down Pillow 羽绒枕", "Camping Tent 露营帐篷",
	"Hiking Backpack 登山包", "Vitamin Serum 精华液", "Lip Balm 润唇膏",
	"Picture Book 绘本", "Cookbook 食谱", "Action Figure 手办", "Building Set 拼装套件",
	"Yoga Mat 瑜伽垫", "Resistance Band 弹力带", "Coffee Beans 咖啡豆", "Matcha Powder 抹茶粉",
	"Fountain Pen 钢笔", "Notebook 笔记本", "Terracotta Pot 陶盆", "Watering Can 洒水壶",
	"Denim Jacket 牛仔外套", "Merino Sweater 美利奴毛衣", "Wireless Earbuds 无线耳机",
	"Power Bank 移动电源", "Glass Tumbler 玻璃杯", "Bamboo Steamer 竹蒸笼",
	"Silk Pillowcase 真丝枕套", "Weighted Blanket 重力毯", "Sleeping Bag 睡袋",
	"Insulated Bottle 保温壶", "Sunscreen 防晒霜", "Hand Cream 护手霜",
	"Graphic Novel 图像小说", "Atlas 地图册", "Plush Toy 毛绒玩具", "Puzzle 拼图",
	"Dumbbell 哑铃", "Jump Rope 跳绳", "Loose Leaf Tea 散茶", "Olive Oil 橄榄油",
	"Mechanical Pencil 自动铅笔", "Sticky Notes 便利贴", "Hanging Basket 吊篮", "Garden Shears 园艺剪",
}

var productColors = []string{
	"Charcoal 炭灰", "Ivory 象牙", "Sage 鼠尾草绿", "Terracotta 赤陶", "Navy 海军蓝",
	"Burgundy 勃艮第", "Olive 橄榄绿", "Slate 板岩灰", "Cream 奶白", "Forest 森林绿",
}

var productSizes = []string{"S", "M", "L", "XL"}

// Customer vocabulary: fictional regional surnames, given names, cities, and
// streets. None of these correspond to real individuals; they exist solely to
// produce realistic-looking seeded rows.
var customerRegions = []struct {
	Country  string
	Code     string // ISO-3166 alpha-2
	Region   string
	City     string
	District string
	Currency string
}{
	{"China", "CN", "Shanghai", "上海", "浦东新区", "CNY"},
	{"China", "CN", "Beijing", "北京", "朝阳区", "CNY"},
	{"China", "CN", "Guangdong", "深圳", "南山区", "CNY"},
	{"United States", "US", "California", "San Francisco", "SoMa", "USD"},
	{"United States", "US", "New York", "New York", "Manhattan", "USD"},
	{"United States", "US", "Oregon", "Portland", "Pearl District", "USD"},
	{"United Kingdom", "GB", "England", "London", "Camden", "GBP"},
	{"United Kingdom", "GB", "Scotland", "Edinburgh", "Old Town", "GBP"},
}

var customerSurnamesCN = []string{"王", "李", "张", "刘", "陈", "杨", "黄", "赵", "周", "吴"}
var customerGivenNamesCN = []string{"伟", "芳", "娜", "敏", "静", "强", "磊", "军", "洋", "勇"}
var customerSurnamesEN = []string{"Walker", "Bennett", "Carter", "Davies", "Ellis", "Foster", "Griffin", "Harris", "Ingram", "Jenkins"}
var customerGivenNamesEN = []string{"Alex", "Bailey", "Casey", "Drew", "Emery", "Finley", "Hayden", "Jordan", "Morgan", "Quinn"}

var streetNamesCN = []string{"世纪大道", "建国路", "深南大道", "中山路", "人民路"}
var streetNamesEN = []string{"Market St", "5th Ave", "Burnside St", "High St", "Royal Mile"}

// membershipTiers are the curated tier ladder. The minimum-spend thresholds are
// in minor units (CNY/USD/GBP-equivalent minor units).
var membershipTiers = []struct {
	ID                string
	Name              string
	Rank              int
	MinimumSpendMinor int64
}{
	{"tier-bronze", "Bronze 铜卡", 0, 0},
	{"tier-silver", "Silver 银卡", 1, 50000},
	{"tier-gold", "Gold 金卡", 2, 200000},
}

// paymentProviders and carriers are the curated channel vocabulary.
var paymentProviders = []struct {
	Name    string
	Methods []string
}{
	{"stripe", []string{"card"}},
	{"alipay", []string{"wallet"}},
	{"paypal", []string{"card", "wallet"}},
	{"adyen", []string{"card", "bank_transfer"}},
}

var carriers = []string{"SF Express 顺丰", "UPS", "DHL", "FedEx", "Royal Mail", "China Post 中国邮政"}

var failureReasons = []string{
	"insufficient_funds",
	"card_declined",
	"provider_timeout",
	"risk_rejection",
}

var refundReasons = []string{
	"customer_request",
	"damaged_in_transit",
	"wrong_item_shipped",
	"order_cancelled",
	"quality_issue",
}
