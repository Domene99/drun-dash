//go:build desktop && darwin

// This file adds a native macOS menu bar with a standard Edit menu to the
// desktop window. webview_go creates the NSApplication + NSWindow but installs
// no main menu, so the standard editing key equivalents (Cmd+C/V/X/A) have no
// menu item to route them and never fire — leaving the member unable to copy the
// agent's replies or paste into fields. Installing an Edit menu whose items use
// the standard first-responder selectors (copy:/paste:/cut:/selectAll:) makes
// the focused WKWebView handle them, so select-and-copy and paste work app-wide
// like any native Mac app. It carries the "desktop" build tag (cgo webview) and
// is darwin-only (the _darwin suffix); other platforms get their own menus via
// their toolkits if/when packaged.
package main

/*
#cgo CFLAGS: -x objective-c
#cgo LDFLAGS: -framework Cocoa
#import <Cocoa/Cocoa.h>

// installEditMenu builds a minimal, well-formed menu bar: an application menu
// (so Cmd+Q works and the bar is valid) and an Edit menu wired to the standard
// responder-chain actions. Called on the main thread after the webview created
// the shared NSApplication.
static void installEditMenu(void) {
    NSApplication *app = [NSApplication sharedApplication];

    NSMenu *menubar = [[NSMenu alloc] init];
    [app setMainMenu:menubar];

    // Application menu — first item; provides Quit and makes the bar well-formed.
    NSMenuItem *appMenuItem = [[NSMenuItem alloc] init];
    [menubar addItem:appMenuItem];
    NSMenu *appMenu = [[NSMenu alloc] init];
    [appMenuItem setSubmenu:appMenu];
    NSString *appName = [[NSProcessInfo processInfo] processName];
    [appMenu addItemWithTitle:[@"Quit " stringByAppendingString:appName]
                       action:@selector(terminate:)
                keyEquivalent:@"q"];

    // Edit menu — standard actions; the responder chain routes them to the
    // focused WKWebView (selectable text / text fields).
    NSMenuItem *editMenuItem = [[NSMenuItem alloc] init];
    [menubar addItem:editMenuItem];
    NSMenu *editMenu = [[NSMenu alloc] initWithTitle:@"Edit"];
    [editMenuItem setSubmenu:editMenu];

    [editMenu addItemWithTitle:@"Undo" action:@selector(undo:) keyEquivalent:@"z"];
    NSMenuItem *redo = [editMenu addItemWithTitle:@"Redo" action:@selector(redo:) keyEquivalent:@"z"];
    [redo setKeyEquivalentModifierMask:(NSEventModifierFlagCommand | NSEventModifierFlagShift)];
    [editMenu addItem:[NSMenuItem separatorItem]];
    [editMenu addItemWithTitle:@"Cut" action:@selector(cut:) keyEquivalent:@"x"];
    [editMenu addItemWithTitle:@"Copy" action:@selector(copy:) keyEquivalent:@"c"];
    [editMenu addItemWithTitle:@"Paste" action:@selector(paste:) keyEquivalent:@"v"];
    [editMenu addItemWithTitle:@"Select All" action:@selector(selectAll:) keyEquivalent:@"a"];
}
*/
import "C"

// installNativeEditMenu installs the macOS Edit menu so standard copy/paste/cut/
// select-all work in the window. Safe to call once, on the main thread, after
// the webview has created the NSApplication and before Run.
func installNativeEditMenu() { C.installEditMenu() }
