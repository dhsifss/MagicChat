import assert from "node:assert/strict"
import { readFile } from "node:fs/promises"
import test from "node:test"
import { runInNewContext } from "node:vm"
import ts from "typescript"
import ExternalLinkIcon from "@hugeicons/core-free-icons/dist/esm/ExternalLinkIcon"
import MessageMultiple02Icon from "@hugeicons/core-free-icons/dist/esm/MessageMultiple02Icon"
import Volume02Icon from "@hugeicons/core-free-icons/dist/esm/Volume02Icon"
import PlayCircle02Icon from "@hugeicons/core-free-icons/dist/esm/PlayCircle02Icon"
import PauseCircleIcon from "@hugeicons/core-free-icons/dist/esm/PauseCircleIcon"

import { formatVoicePlaybackTime } from "../src/features/conversation/voice/voice-message-presentation.ts"
import { cardSummary } from "../../shared/card-summary.ts"
import { parseTaskLink } from "../../shared/task-links.ts"
import { parseContactLink } from "../../shared/contact-links.ts"

const source = (path: string) => readFile(new URL(`../src/${path}`, import.meta.url), "utf8")

test("链接与记录使用Desktop同款主色28px图标和两行单行文字", async () => {
  const body = await source("features/conversation/messages/message-body.tsx")
  const link = body.split("function MessageLinkBody(")[1].split("function MessageLinkCard(")[0]
  const forward = body.split("function ForwardBundleBody(")[1].split("function getImageDisplaySize(")[0]
  for (const [content, kind] of [[link, "link"], [forward, "forward"]]) {
    assert.ok(content.includes(`color={colors.textPrimary} kind="${kind}" size={28}`))
    assert.match(content, /alignItems: "center", flexDirection: "row", gap: 12, width: "100%"/)
    assert.match(content, /flex: 1, minWidth: 0/)
    assert.match(content, /numberOfLines=\{1\}[\s\S]*?color: colors\.textPrimary, fontSize: 14, lineHeight: 24/)
    assert.match(content, /numberOfLines=\{1\}[\s\S]*?color: colors\.textSecondary, fontSize: 12, lineHeight: 16/)
    assert.doesNotMatch(content, /brand5|numberOfLines=\{[24]\}/)
  }
  assert.match(link, /\{title\}[\s\S]*?\{url\}/)
  assert.match(forward, /body\.itemCount.*firstSummary/)
})

test("链接仍外部打开、长按不触发链接，普通卡片保持独立渲染", async () => {
  const body = await source("features/conversation/messages/message-body.tsx")
  assert.match(body, /<MessageLinkBody[\s\S]*?onPress=\{\(\) => void openExternalUrl\(body\.url\)\}/)
  const link = body.split("function MessageLinkBody(")[1].split("function MessageLinkCard(")[0]
  assert.match(link, /accessibilityRole="link"/)
  assert.match(link, /if \(didLongPressRef\.current\) \{\s*didLongPressRef\.current = false\s*return\s*\}\s*onPress\(\)/)
  assert.match(body, /body\.type === "card"[\s\S]*?<MessageLinkCard/)
  assert.match(body, /await Linking\.openURL\(url\)/)
})

test("链接标题右侧提供独立复制按钮，复制完整URL并显示结果", async () => {
  const body = await source("features/conversation/messages/message-body.tsx")
  const link = body.split("function MessageLinkBody(")[1].split("function MessageLinkCard(")[0]
  assert.match(link, /await Clipboard\.setStringAsync\(url\)/)
  assert.match(link, /message: "链接已复制"/)
  assert.match(link, /message: "复制链接失败"/)
  assert.match(link, /lineHeight: 24, paddingRight: 32/)
  assert.match(link, /<\/Pressable>\s*<Pressable\s*accessibilityLabel="复制链接"/)
  assert.match(link, /accessibilityRole="button"\s*hitSlop=\{10\}\s*onPress=\{\(\) => void copyLink\(\)\}/)
  assert.match(link, /position: "absolute", right: 0, top: 0/)
  assert.match(link, /kind="copy" size=\{14\} strokeWidth=\{1\.5\}/)
})

test("普通卡片样式对齐Desktop，保留宽度与长按/点击交互", async () => {
  const body = await source("features/conversation/messages/message-body.tsx")
  const ast = ts.createSourceFile("body.tsx", body, ts.ScriptTarget.Latest, true, ts.ScriptKind.TSX)
  const fn = ast.statements.find(node => ts.isFunctionDeclaration(node) && node.name?.text === "MessageLinkCard")!
  const compiled = ts.transpileModule(fn.getText(ast) + "\nexports.render = MessageLinkCard", {
    compilerOptions: { jsx: ts.JsxEmit.React, target: ts.ScriptTarget.ES2022, module: ts.ModuleKind.CommonJS },
  }).outputText
  const exports: any = {}
  const colors = { textPrimary: "primary", textSecondary: "secondary" }
  runInNewContext(compiled, {
    exports, React: { createElement: (type: string, props: any, ...children: any[]) => ({ type, props, children }) },
    Pressable: "Pressable", View: "View", Text: "Text", MessageContentIcon: "Icon",
    useXGUITheme: () => ({ colors }), useRef: (current: boolean) => ({ current }),
    cardSummary, parseTaskLink, parseContactLink,
  })
  let clicks = 0, longPresses = 0
  const card = exports.render({ url: "https://example.test/card", title: "title", description: "summary", onPress: () => clicks++, onLongPress: () => longPresses++ })
  assert.equal(card.props.style.gap, 8)
  assert.equal(card.props.style.width, "100%")
  assert.equal(card.props.style.flexDirection, "column")
  const text = (node: any): any[] => !node || typeof node !== "object" ? [] : [...(node.type === "Text" ? [node] : []), ...(node.children ?? []).flatMap(text)]
  const [title, summary] = text(card)
  assert.deepEqual(JSON.parse(JSON.stringify(title.props.style)), { color: "primary", fontSize: 15, fontWeight: "500", lineHeight: 23 })
  assert.equal(summary.props.numberOfLines, 3)
  assert.equal(summary.props.style.color, "secondary")
  card.props.onLongPress(); card.props.onPress()
  assert.equal(longPresses, 1)
  assert.equal(clicks, 0)
  card.props.onPressIn(); card.props.onPress()
  assert.equal(clicks, 1)
  const handler = body.slice(body.indexOf("onPress={body.url.trim()"), body.indexOf("title={cardDisplayTitle"))
  assert.match(handler, /onPress=\{body\.url\.trim\(\) \? \(\) => \{/)
  assert.match(handler, /else void openExternalUrl\(body\.url\)/)
  assert.match(handler, /\} : undefined\}/)
})

test("共享图标正确保留Desktop圆形播放按钮的圆环和实心三角", async () => {
  for (const icon of [ExternalLinkIcon, MessageMultiple02Icon, Volume02Icon, PlayCircle02Icon, PauseCircleIcon]) {
    assert.ok(icon.every(([tag]) => tag === "path" || tag === "circle"))
  }
  assert.equal(PlayCircle02Icon[0][0], "circle")
  assert.equal(PlayCircle02Icon[1][1].fill, "currentColor")
  assert.equal(PauseCircleIcon[0][0], "circle")
  const icon = await source("features/conversation/messages/message-content-icon.tsx")
  assert.match(icon, /tag === "circle" \?[\s\S]*?<Circle/)
  assert.match(icon, /fill: attributes\.fill === "currentColor" \? color : "none"/)
  for (const name of ["ExternalLinkIcon", "MessageMultiple02Icon", "Volume02Icon", "PlayCircle02Icon", "PauseCircleIcon"]) {
    assert.ok(icon.includes(`from "@hugeicons/core-free-icons/dist/esm/${name}"`))
  }
})

test("语音显示总时长或播放进度，时间与Desktop按秒向下取整一致", () => {
  for (const [value, expected] of [[0, "0:00"], [999, "0:00"], [1_999, "0:01"], [61_200, "1:01"], [-100, "0:00"]] as const) {
    assert.equal(formatVoicePlaybackTime(value), expected)
  }
})

test("语音布局和加载/播放/暂停按钮与文件气泡共享按下反馈", async () => {
  const voice = await source("features/conversation/voice/voice-message-player.tsx")
  assert.match(voice, /kind="voice" size=\{28\}/)
  assert.match(voice, /语音 - \{formatVoicePlaybackTime\(displayedDuration\)\}/)
  assert.match(voice, /transcript \|\| "暂无文字摘要"/)
  assert.match(voice, /numberOfLines=\{1\}/)
  assert.match(voice, /<MessageActionButton[\s\S]*?kind=\{isLoading \? "loading" : playerStatus\.playing \? "pause" : "play"\}/)
  assert.match(voice, /strokeWidth=\{isLoading \? 1\.5 : 2\}/)
  assert.match(voice, /disabled=\{isLoading\}/)
  assert.doesNotMatch(voice, /NativeIconButton|AudioLines|XGUILoadingIcon/)
})

test("语音继续互斥播放、自动下载播放和重试，并清理新增状态订阅", async () => {
  const voice = await source("features/conversation/voice/voice-message-player.tsx")
  assert.match(voice, /playerStatus\.currentTime \* 1_000/)
  assert.match(voice, /activateVoicePlayer\(\{ id: playerId, pause: \(\) => player\.pause\(\) \}\)/)
  assert.match(voice, /if \(!resourceUri\) \{\s*playWhenReadyRef\.current = true\s*onResourceRequest\(fileId\)/)
  assert.match(voice, /onResourceError\(fileId\)/)
  assert.match(voice, /player\.addListener\("playbackStatusUpdate"/)
  assert.match(voice, /status\.didJustFinish \|\| status\.error[\s\S]*?setHasPlaybackStarted\(false\)/)
  assert.match(voice, /return \(\) => subscription\.remove\(\)/)
  assert.match(voice, /await player\.seekTo\(0\)/)
  assert.match(voice, /if \(didLongPressRef\.current\)[\s\S]*?return[\s\S]*?if \(isLoading\) return/)
})
