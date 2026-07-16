/*
 * theme-toggle.js — the only JavaScript the design system ships.
 *
 * No framework, no build step. It wires three progressively-enhanced behaviors
 * via event delegation on the document, so it works for markup added later by
 * HTMX without re-binding:
 *
 *   [data-uk-theme-toggle]        flip light/dark and persist to localStorage
 *   [data-uk-modal-open="<id>"]   open the <dialog id> as a focus-trapped modal
 *   [data-uk-modal-close]         close the enclosing <dialog>
 *   [data-uk-copy="<text>"]       copy text to the clipboard (ID chip)
 *
 * The INITIAL theme is applied by a tiny inline script in the <head> (see the
 * Head templ component) before first paint, so there is no flash. This file
 * only handles interaction after load.
 */
(function () {
	"use strict";

	var THEME_KEY = "uikit-theme";

	function currentTheme() {
		var explicit = document.documentElement.getAttribute("data-theme");
		if (explicit === "dark" || explicit === "light") {
			return explicit;
		}
		return window.matchMedia && window.matchMedia("(prefers-color-scheme: dark)").matches
			? "dark"
			: "light";
	}

	function applyTheme(theme) {
		document.documentElement.setAttribute("data-theme", theme);
		try {
			localStorage.setItem(THEME_KEY, theme);
		} catch (e) {
			/* storage may be unavailable (private mode) — theme still applies */
		}
	}

	function toggleTheme() {
		applyTheme(currentTheme() === "dark" ? "light" : "dark");
	}

	document.addEventListener("click", function (event) {
		var toggle = event.target.closest("[data-uk-theme-toggle]");
		if (toggle) {
			event.preventDefault();
			toggleTheme();
			return;
		}

		var opener = event.target.closest("[data-uk-modal-open]");
		if (opener) {
			var dialog = document.getElementById(opener.getAttribute("data-uk-modal-open"));
			if (dialog && typeof dialog.showModal === "function") {
				event.preventDefault();
				dialog.showModal();
			}
			return;
		}

		var closer = event.target.closest("[data-uk-modal-close]");
		if (closer) {
			var owner = closer.closest("dialog");
			if (owner) {
				event.preventDefault();
				owner.close();
			}
			return;
		}

		var copier = event.target.closest("[data-uk-copy]");
		if (copier && navigator.clipboard) {
			event.preventDefault();
			navigator.clipboard.writeText(copier.getAttribute("data-uk-copy")).then(function () {
				copier.classList.add("is-copied");
				setTimeout(function () {
					copier.classList.remove("is-copied");
				}, 1200);
			}, function () {
				/* clipboard denied — nothing to do */
			});
		}
	});
})();
