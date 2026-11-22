# Prism Muse plugin

A macOS PluginBridge v4 adapter for the personal Muse at `muse.ai`. The Go adapter and native WKWebView host ship together. Node.js, Python, Chrome and a separate service are unnecessary at runtime.

Build with Go and the Xcode command-line tools.

```sh
./build.sh
```

Install the `dist` folder from Prism Hub. In a Hub build with the Muse WebView action, open the Muse card’s menu and choose **Open WebView**. Sign in to **Muse Web**, then enable remote access. In Prism Panel, open **Muse → Prism Muse → 主要聊天**.

Prism lists the main chat and the side chats found in the Muse sidebar. A new unbound Prism send goes to the main chat. Selecting a side chat opens its own history and sends replies to that chat. The adapter verifies each new user message in the selected chat before acknowledging the send.

Approvals remain in Muse Web. The plugin handles text and serializes submissions.
