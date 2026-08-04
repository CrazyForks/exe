#import <Cocoa/Cocoa.h>
#import <CoreGraphics/CoreGraphics.h>
#include <stdlib.h>
#include "_cgo_export.h"

static NSStatusItem *statusItem;

@interface ExeMenuTarget : NSObject
@end

@implementation ExeMenuTarget
- (void)openUI:(id)sender {
	goMenuOpenUI();
}
- (void)restartDaemon:(id)sender {
	goMenuRestart();
}
- (void)quitDaemon:(id)sender {
	char *cmsg = goMenuQuitMessage();
	NSString *msg = [NSString stringWithUTF8String:cmsg];
	free(cmsg);
	NSAlert *alert = [[NSAlert alloc] init];
	alert.messageText = @"Quit exe?";
	alert.informativeText = msg;
	[alert addButtonWithTitle:@"Quit"];
	[alert addButtonWithTitle:@"Cancel"];
	[NSApp activateIgnoringOtherApps:YES];
	if ([alert runModal] == NSAlertFirstButtonReturn) {
		goMenuQuit();
	}
}
@end

static ExeMenuTarget *target;

int menubarSupported(void) {
	CFDictionaryRef session = CGSessionCopyCurrentDictionary();
	if (session == NULL) {
		return 0;
	}
	CFRelease(session);
	return 1;
}

static NSMenuItem *item(NSString *title, SEL action) {
	NSMenuItem *it = [[NSMenuItem alloc] initWithTitle:title action:action keyEquivalent:@""];
	it.target = target;
	return it;
}

void menubarRun(void) {
	[NSApplication sharedApplication];
	[NSApp setActivationPolicy:NSApplicationActivationPolicyAccessory];
	target = [[ExeMenuTarget alloc] init];

	statusItem = [[NSStatusBar systemStatusBar] statusItemWithLength:NSSquareStatusItemLength];
	NSImage *icon = [NSImage imageWithSystemSymbolName:@"server.rack" accessibilityDescription:@"exe"];
	if (icon != nil) {
		[icon setTemplate:YES];
		statusItem.button.image = icon;
	} else {
		statusItem.button.title = @"exe";
	}
	statusItem.button.toolTip = @"exe — personal VM cloud";

	NSMenu *menu = [[NSMenu alloc] init];
	[menu addItem:item(@"Open Web UI", @selector(openUI:))];
	[menu addItem:item(@"Restart Daemon", @selector(restartDaemon:))];
	[menu addItem:[NSMenuItem separatorItem]];
	[menu addItem:item(@"Quit exe…", @selector(quitDaemon:))];
	statusItem.menu = menu;

	[NSApp run];
}
