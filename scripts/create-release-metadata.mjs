import { createHash } from "node:crypto";
import { readdir, readFile, writeFile } from "node:fs/promises";
import { resolve } from "node:path";
import { fileURLToPath } from "node:url";

const root = fileURLToPath(new URL("..", import.meta.url));
const artifacts = resolve(root, process.argv[2] || "release-artifacts");
const manifest = await readFile(resolve(root, "pluginbridge-plugin.yaml"), "utf8");
const field = (name) => manifest.match(new RegExp(`^${name}:\\s*(.+)$`, "m"))?.[1]?.trim().replace(/^(?:\"|')|(?:\"|')$/g, "");
const id = field("id");
const version = field("version");
const displayName = field("display_name");
const description = field("description");
const protocolVersion = field("plugin_protocol_version");
const minHubVersion = field("min_hub_version");
const iconURL = field("icon_url");
const iconSVGURL = field("icon_svg_url");
const platforms = ["macos-arm64", "macos-x64"];

if (id !== "muse" || !/^\d+\.\d+\.\d+$/.test(version || "") || !displayName || !protocolVersion || !minHubVersion || !iconURL) {
  throw new Error("插件清单缺少有效的发布信息");
}
if (process.env.GITHUB_REF_TYPE === "tag" && process.env.GITHUB_REF_NAME !== `v${version}`) {
  throw new Error("版本标签与插件清单版本不一致");
}

const names = (await readdir(artifacts)).filter((name) => name.endsWith(".tar.gz"));
if (names.length !== platforms.length) throw new Error(`发布包数量错误：需要 ${platforms.length} 个，实际 ${names.length} 个`);

const assets = await Promise.all(platforms.map(async (platform) => {
  const filename = `${id}-${version}-${platform}.tar.gz`;
  if (!names.includes(filename)) throw new Error(`缺少目标平台发布包：${filename}`);
  const bytes = await readFile(resolve(artifacts, filename));
  if (!bytes.length) throw new Error(`发布包为空：${filename}`);
  return {
    platform,
    filename,
    sha256: createHash("sha256").update(bytes).digest("hex"),
    size_bytes: bytes.length,
  };
}));

const metadata = {
  schema_version: 1,
  plugin_id: id,
  version,
  display_name: displayName,
  description,
  icon_url: iconURL,
  ...(iconSVGURL ? { icon_svg_url: iconSVGURL } : {}),
  channel: "stable",
  plugin_protocol_version: protocolVersion,
  min_hub_version: minHubVersion,
  supported_platforms: platforms,
  assets,
};

await writeFile(resolve(artifacts, "plugin-release.json"), JSON.stringify(metadata, null, 2) + "\n");
