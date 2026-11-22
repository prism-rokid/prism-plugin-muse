import AppKit
import WebKit

struct Command: Decodable {
    let operation: String
    let path: String?
    let text: String?
}

final class MuseWeb: NSObject, NSApplicationDelegate, WKUIDelegate, WKNavigationDelegate, NSWindowDelegate {
    private var window: NSWindow!
    private var web: WKWebView!
    private var popups: [NSWindow] = []
    private let managed = CommandLine.arguments.contains("--plugin-worker")
    private let origin = URL(string: "https://muse.ai/")!
    private var waitingToPresentPage = false
    private var backgroundPresentation: DispatchWorkItem?
    private var presentingCatalog = false
    private var nextCatalogWake: TimeInterval = 0

    func applicationDidFinishLaunching(_ notification: Notification) {
        let config = WKWebViewConfiguration()
        config.websiteDataStore = .default()
        if managed, #available(macOS 14.0, *) {
            config.preferences.inactiveSchedulingPolicy = .none
        }
        web = WKWebView(frame: .zero, configuration: config)
        web.uiDelegate = self
        web.navigationDelegate = self
        window = makeWindow(title: "Muse Web", view: web, width: 1060, height: 740)
        window.delegate = self
        loadChat("/")
        guard managed else {
            show()
            return
        }
        DispatchQueue.global(qos: .userInitiated).async {
            while let line = readLine() {
                DispatchQueue.main.async { self.handle(line) }
            }
            DispatchQueue.main.async { NSApp.terminate(nil) }
        }
    }

    private func makeWindow(title: String, view: WKWebView, width: CGFloat, height: CGFloat) -> NSWindow {
        let result = NSWindow(contentRect: NSRect(x: 0, y: 0, width: width, height: height),
                              styleMask: [.titled, .closable, .miniaturizable, .resizable],
                              backing: .buffered, defer: false)
        result.title = title
        result.contentView = view
        result.isReleasedWhenClosed = false
        result.center()
        return result
    }

    private func show() {
        cancelBackgroundPresentation()
        if window.isMiniaturized { window.deminiaturize(nil) }
        window.makeKeyAndOrderFront(nil)
        NSApp.activate(ignoringOtherApps: true)
    }

    private func loadChat(_ path: String) {
        finishBackgroundPresentation()
        waitingToPresentPage = managed && !window.isVisible
        web.load(URLRequest(url: URL(string: path, relativeTo: origin)!))
    }

    func webView(_ webView: WKWebView, didFinish navigation: WKNavigation!) {
        guard waitingToPresentPage, webView.url?.host == origin.host else { return }
        waitingToPresentPage = false
        presentInBackground(forCatalog: false)
    }

    private func presentInBackground(forCatalog: Bool) {
        guard managed, !window.isVisible else { return }
        presentingCatalog = forCatalog
        // Muse waits for a visible page before mounting the chat.
        window.alphaValue = 0
        window.ignoresMouseEvents = true
        window.orderFrontRegardless()
        let finish = DispatchWorkItem { [weak self] in self?.finishBackgroundPresentation() }
        backgroundPresentation = finish
        DispatchQueue.main.asyncAfter(deadline: .now() + (forCatalog ? 8 : 5), execute: finish)
    }

    private func cancelBackgroundPresentation() {
        waitingToPresentPage = false
        backgroundPresentation?.cancel()
        backgroundPresentation = nil
        presentingCatalog = false
        window.alphaValue = 1
        window.ignoresMouseEvents = false
    }

    private func finishBackgroundPresentation() {
        guard backgroundPresentation != nil else { return }
        window.orderOut(nil)
        cancelBackgroundPresentation()
    }

    func webViewWebContentProcessDidTerminate(_ webView: WKWebView) {
        guard webView === web else { return }
        finishBackgroundPresentation()
        waitingToPresentPage = managed && !window.isVisible
        webView.reload()
    }

    func windowDidBecomeKey(_ notification: Notification) {
        guard notification.object as? NSWindow === window else { return }
        cancelBackgroundPresentation()
    }

    func applicationShouldTerminateAfterLastWindowClosed(_ sender: NSApplication) -> Bool {
        !managed
    }

    func windowShouldClose(_ sender: NSWindow) -> Bool {
        guard managed, sender === window else { return true }
        if backgroundPresentation != nil {
            finishBackgroundPresentation()
        } else {
            cancelBackgroundPresentation()
            sender.orderOut(nil)
        }
        return false
    }

    func windowWillClose(_ notification: Notification) {
        popups.removeAll { $0 === notification.object as? NSWindow }
    }

    func applicationShouldHandleReopen(_ sender: NSApplication, hasVisibleWindows: Bool) -> Bool {
        show()
        return true
    }

    private func answer(_ result: [String: Any]) {
        guard let data = try? JSONSerialization.data(withJSONObject: result),
              let line = String(data: data, encoding: .utf8) else {
            print("{\"error\":\"Muse returned an unreadable response\"}")
            fflush(stdout)
            return
        }
        print(line)
        fflush(stdout)
    }

    private func handle(_ line: String) {
        guard let command = try? JSONDecoder().decode(Command.self, from: Data(line.utf8)) else {
            answer(["error": "Invalid Muse command"])
            return
        }
        switch command.operation {
        case "show":
            show()
            answer(["ok": true])
        case "navigate":
            guard let path = command.path, path == "/" || Self.isSideChatPath(path) else {
                answer(["error": "Invalid Muse page path"])
                return
            }
            loadChat(path)
            answer(["ok": true])
        case "catalog":
            guard web.url?.scheme == "https", web.url?.host == origin.host else {
                answer([:])
                return
            }
            // Read Muse's active directory independently of the visible sidebar page.
            web.callAsyncJavaScript(Self.catalog, arguments: [:], in: nil, in: .page) { result in
                let response = (try? result.get()) as? [String: Any] ?? [:]
                if response["catalog_complete"] as? Bool == true, self.presentingCatalog {
                    self.finishBackgroundPresentation()
                }
                if response["catalog_needs_visibility"] as? Bool == true, self.managed, !self.window.isVisible {
                    let now = ProcessInfo.processInfo.systemUptime
                    if now >= self.nextCatalogWake {
                        self.nextCatalogWake = now + 15
                        self.presentInBackground(forCatalog: true)
                    }
                }
                self.answer(response)
            }
        case "snapshot", "submit":
            guard web.url?.scheme == "https", web.url?.host == origin.host else {
                if command.operation == "submit" {
                    answer(["error": "Muse is not showing the selected chat", "not_submitted": true])
                } else {
                    answer(["ready": false])
                }
                return
            }
            web.callAsyncJavaScript(Self.pageOperations, arguments: [
                "operation": command.operation, "expectedPath": command.path ?? "",
                "prompt": command.text ?? ""
            ], in: nil, in: .defaultClient) { result in
                switch result {
                case .success(let object):
                    var response = object as? [String: Any] ?? ["error": "Muse response is missing"]
                    if response["ready"] as? Bool == true && !self.presentingCatalog {
                        self.finishBackgroundPresentation()
                    }
                    response["window_open"] = self.window.isVisible && self.window.alphaValue > 0
                    self.answer(response)
                case .failure:
                    self.answer(["error": "Muse page was unavailable during the operation"])
                }
            }
        default:
            answer(["error": "Unknown Muse operation"])
        }
    }

    private static func isSideChatPath(_ path: String) -> Bool {
        guard path.hasPrefix("/thread/") else { return false }
        return UUID(uuidString: String(path.dropFirst("/thread/".count))) != nil
    }

    private static let catalog = #"""
    const idPattern = /^[a-f0-9]{8}(?:-[a-f0-9]{4}){3}-[a-f0-9]{12}$/i;
    const path = location.pathname;
    const panel = document.querySelector('[data-testid="hatch-side-chats-panel-content"]');
    if (!panel) return {path};

    const fiberFor = (el) => el?.[Object.keys(el).find(key => key.startsWith('__reactFiber$'))];
    const clients = new Set();
    const scopes = new Set();
    for (let fiber = fiberFor(panel), depth = 0; fiber && depth < 40; fiber = fiber.return, depth++) {
        for (let dep = fiber.dependencies?.firstContext; dep; dep = dep.next) {
            const value = dep.memoizedValue;
            if (typeof value?.getQueryCache === 'function') clients.add(value);
            if (value?.isReady === true && value.connectionState === 'connected' &&
                typeof value.gatewayUrl === 'string' && value.gatewayUrl && typeof value.vmName === 'string') {
                scopes.add(JSON.stringify([value.gatewayUrl, value.vmName]));
            }
        }
    }

    const titles = new Map();
    const compose = panel.querySelector('[data-testid="hatch-chat-compose"]');
    // The archive view can render the same row component as the active list.
    for (const row of compose ? panel.querySelectorAll('[data-testid="hatch-thread-row"]') : []) {
        let fiber = fiberFor(row);
        let title = '';
        let updated = 0;
        for (let depth = 0; fiber && depth < 40; depth++, fiber = fiber.return) {
            const props = fiber.memoizedProps;
            if (!title && typeof props?.title === 'string') {
                title = props.title.trim().slice(0, 200);
                updated = milliseconds(props.timestamp);
            }
            if (idPattern.test(fiber.key ?? '')) {
                if (title) titles.set(fiber.key, {thread: fiber.key, title, updated});
                break;
            }
        }
    }
    const partial = {path, chats: [...titles.values()]};
    const pending = {...partial, catalog_needs_visibility: document.hidden};
    if (clients.size !== 1 || scopes.size !== 1) return pending;
    const [client] = clients;
    const [scope] = scopes;
    const queries = client.getQueryCache().getAll().filter(query => {
        const key = query.queryKey;
        return Array.isArray(key) && key.length === 2 && key[0] === 'thread-sessions' &&
            key[1] === scope && query.isActive();
    });
    if (queries.length !== 1) return pending;
    const [query] = queries;
    const state = query.state;
    // Changes from another Muse window may not invalidate this WebView's cache.
    if (state.fetchStatus === 'idle' && Date.now() - Math.max(state.dataUpdatedAt, state.errorUpdatedAt) >= 20000) {
        if (document.hidden) return pending;
        void query.fetch(undefined, {cancelRefetch: false}).catch(() => {});
        return pending;
    }
    if (state.status !== 'success' || state.fetchStatus !== 'idle' || state.isInvalidated ||
        !Array.isArray(state.data)) return pending;

    const chats = [];
    for (const item of state.data) {
        if (typeof item?.is_primary !== 'boolean' || typeof item.is_thread !== 'boolean' ||
            typeof item.archived !== 'boolean') return partial;
        if (item.is_primary || !item.is_thread || item.archived) continue;
        if (typeof item.session_id !== 'string' || !idPattern.test(item.session_id)) return partial;
        const thread = item.session_id;
        const title = titles.get(thread)?.title || (typeof item.title === 'string' && item.title.trim()) || '旁聊';
        const updated = milliseconds(item.updated_at);
        chats.push({thread, title: title.slice(0, 200), updated});
    }
    return {path, chats, catalog_complete: true};

    function milliseconds(value) {
        if (typeof value === 'number' && Number.isFinite(value)) {
            return Math.trunc(value < 1e12 ? value * 1000 : value);
        }
        return typeof value === 'string' ? (Date.parse(value) || 0) : 0;
    }
    """#

    func webView(_ webView: WKWebView, createWebViewWith configuration: WKWebViewConfiguration,
                 for navigationAction: WKNavigationAction, windowFeatures: WKWindowFeatures) -> WKWebView? {
        let child = WKWebView(frame: .zero, configuration: configuration)
        child.uiDelegate = self
        let popup = makeWindow(title: "Muse Web – Sign In", view: child, width: 780, height: 680)
        popup.delegate = self
        popup.makeKeyAndOrderFront(nil)
        popups.append(popup)
        return child
    }

    func webViewDidClose(_ webView: WKWebView) {
        if let i = popups.firstIndex(where: { $0.contentView === webView }) { popups.remove(at: i).close() }
    }

    private static let pageOperations = #"""
    if (location.origin !== 'https://muse.ai') return {ready: false};
    const path = location.pathname;
    const chat = path === "/" || path === "/thread/main" || /^\/thread\/[a-f0-9]{8}(?:-[a-f0-9]{4}){3}-[a-f0-9]{12}$/i.test(path);
    const select = (selector) => [...document.querySelectorAll(selector)].filter(el =>
        el.getClientRects().length > 0 && !el.closest('[inert],[aria-hidden="true"]'));
    const inputs = select('[contenteditable="true"],textarea');
    const needsSignIn = /^\/(?:login|signin|sign-in|auth)(?:\/|$)/i.test(path) ||
        select('input[autocomplete="one-time-code"],input[autocomplete="current-password"]').length > 0;
    const stops = select('[data-testid="hatch-composer-stop-button"]');
    if (operation === 'snapshot') {
        const ignored = 'button,[role="button"],[aria-hidden="true"],[data-message-accessibility-surrogate],.sr-only,script,style';
        const blocks = new Set(['BLOCKQUOTE', 'DIV', 'H1', 'H2', 'H3', 'H4', 'H5', 'H6', 'P', 'SECTION', 'TABLE', 'TR']);
        const renderText = (node, formatted) => {
            if (node.nodeType === Node.TEXT_NODE) {
                // Prism uses Markdown for replies, so preserve native single line breaks.
                return formatted ? node.nodeValue.replace(/(\S)[\t ]*\n(?=[\t ]*\S)/g, '$1  \n') : node.nodeValue;
            }
            if (node.nodeType !== Node.ELEMENT_NODE || node.matches(ignored)) return '';
            const tag = node.tagName;
            if (tag === 'BR') return formatted ? '  \n' : '\n';
            const formatChildren = formatted && tag !== 'PRE' && tag !== 'CODE';
            const text = [...node.childNodes].map(child => renderText(child, formatChildren)).join('');
            if (tag === 'PRE' && formatted && text.trim()) {
                const tick = String.fromCharCode(96);
                const longest = (text.match(new RegExp(tick + '+', 'g')) || []).reduce((n, run) => Math.max(n, run.length), 2);
                const fence = tick.repeat(longest + 1);
                return '\n\n' + fence + '\n' + text.trimEnd() + '\n' + fence + '\n\n';
            }
            if (tag === 'LI') {
                if (!formatted) return text.trimEnd() + '\n';
                const marker = node.parentElement?.tagName === 'OL'
                    ? ((node.parentElement.start || 1) + [...node.parentElement.children].indexOf(node)) + '. '
                    : '- ';
                return marker + text.trim() + '\n';
            }
            if (tag === 'TD' || tag === 'TH') return text.trim() + '  ';
            return blocks.has(tag) && text.trim() ? text.trimEnd() + '\n\n' : text;
        };
        const messages = new Map();
        for (const el of chat ? select('[data-message-item][data-message-id][data-message-role]').slice(-180) : []) {
            const role = el.getAttribute('data-message-role');
            if (role !== 'assistant' && role !== 'user') continue;
            const body = el.querySelector('[data-hatch-assistant-message-body]') || el;
            messages.set(el.getAttribute('data-message-id'), {
                id: el.getAttribute('data-message-id'), role,
                text: renderText(body, role === 'assistant').trim().slice(0, 80000)
            });
        }
        return {ready: chat && !needsSignIn && inputs.length === 1, needs_sign_in: needsSignIn,
                path, busy: stops.length > 0, messages: [...messages.values()]};
    }
    const rejected = (error) => ({error, not_submitted: true});
    if (!chat || needsSignIn || path !== expectedPath) return rejected('Muse is not showing the selected chat');
    if (inputs.length !== 1 || stops.length > 0) return rejected('Muse composer is unavailable or busy');
    const input = inputs[0];
    const value = () => (input instanceof HTMLTextAreaElement ? input.value : input.innerText).trim();
    if (value()) return rejected('Muse already has an unsent draft');
    const rollback = (draft) => {
        if (draft && location.pathname === expectedPath && input.isConnected &&
            document.activeElement === input && value() === draft) {
            document.execCommand('undo');
        }
    };
    input.focus();
    const inserted = document.execCommand('insertText', false, prompt);
    const draft = value();
    if (!inserted || draft !== prompt.trim()) {
        rollback(draft);
        return rejected('Muse did not accept the message text');
    }
    await new Promise(resolve => setTimeout(resolve, 100));
    if (location.pathname !== expectedPath || !input.isConnected || value() !== prompt.trim())
        return rejected('Muse composer changed before submission');
    const sends = select('[data-hatch-composer-action="send"] button:not(:disabled):not([aria-disabled="true"])');
    if (sends.length !== 1 || select('[data-testid="hatch-composer-stop-button"]').length) {
        rollback(draft);
        return rejected('Muse has no unique Send action');
    }
    sends[0].click();
    return {ok: true};
    """#
}

let app = NSApplication.shared
let delegate = MuseWeb()
app.setActivationPolicy(.accessory)
app.delegate = delegate
app.run()
