"use strict";

// A native <select> popup is drawn by the browser, so its rows cannot be styled
// to match the rest of the interface. On pointer devices we replace it with a
// listbox we draw ourselves; touch devices keep the native control, because the
// OS picker beats anything we could render on a phone.
(function () {
    if (!window.matchMedia("(hover: hover) and (pointer: fine)").matches) return;

    const instances = [];
    let counter = 0;

    function enhance(select) {
        if (select.dataset.listbox) return;
        select.dataset.listbox = "1";

        const wrap = document.createElement("div");
        wrap.className = "listbox";

        const toggle = document.createElement("button");
        toggle.type = "button";
        toggle.className = "listbox-toggle";
        toggle.setAttribute("aria-haspopup", "listbox");
        toggle.setAttribute("aria-expanded", "false");
        const value = document.createElement("span");
        value.className = "listbox-value";
        toggle.append(value);

        const menu = document.createElement("ul");
        menu.className = "listbox-menu";
        menu.id = "listbox-" + ++counter;
        menu.setAttribute("role", "listbox");
        menu.tabIndex = -1;
        menu.hidden = true;
        toggle.setAttribute("aria-controls", menu.id);

        select.parentNode.insertBefore(wrap, select);
        wrap.append(select, toggle, menu);
        select.classList.add("listbox-native");
        select.tabIndex = -1;
        select.setAttribute("aria-hidden", "true");

        let active = -1;
        let typed = "";
        let typedAt = 0;

        function sync() {
            const selected = select.options[select.selectedIndex];
            value.textContent = selected ? selected.textContent : "";
            toggle.disabled = select.disabled || select.options.length === 0;

            menu.replaceChildren();
            [...select.options].forEach((option, index) => {
                const item = document.createElement("li");
                item.id = menu.id + "-" + index;
                item.setAttribute("role", "option");
                item.setAttribute("aria-selected", String(index === select.selectedIndex));
                item.dataset.index = String(index);
                item.textContent = option.textContent;
                if (option.disabled) item.setAttribute("aria-disabled", "true");
                menu.append(item);
            });

            // Rebuilding drops the highlight, so put it back when the list is
            // refreshed while the operator has it open.
            if (!menu.hidden) setActive(active < 0 ? select.selectedIndex : active, false);
        }

        // scroll is suppressed when the pointer drives the highlight, otherwise
        // hovering a half-visible row would nudge the list under the cursor.
        function setActive(index, scroll) {
            const items = [...menu.children];
            if (!items.length) return;
            active = Math.max(0, Math.min(index, items.length - 1));
            items.forEach((item, i) => item.classList.toggle("active", i === active));
            menu.setAttribute("aria-activedescendant", items[active].id);
            if (scroll !== false) items[active].scrollIntoView({ block: "nearest" });
        }

        function open() {
            if (!menu.hidden || toggle.disabled) return;
            sync();
            menu.hidden = false;
            toggle.setAttribute("aria-expanded", "true");
            setActive(select.selectedIndex < 0 ? 0 : select.selectedIndex);
            menu.focus();
        }

        function close(returnFocus) {
            if (menu.hidden) return;
            menu.hidden = true;
            toggle.setAttribute("aria-expanded", "false");
            menu.removeAttribute("aria-activedescendant");
            if (returnFocus !== false) toggle.focus();
        }

        // Only a real change fires the event, so a caller re-selecting the
        // current entry cannot loop back into whatever reloads on change.
        function choose(index) {
            const option = select.options[index];
            if (!option || option.disabled) return;
            if (select.selectedIndex !== index) {
                select.selectedIndex = index;
                select.dispatchEvent(new Event("change", { bubbles: true }));
            }
            sync();
            close();
        }

        function typeahead(character) {
            const now = Date.now();
            typed = now - typedAt > 800 ? character : typed + character;
            typedAt = now;

            const prefix = typed.toLowerCase();
            const match = [...select.options].findIndex((o) => o.textContent.toLowerCase().startsWith(prefix));
            if (match >= 0) setActive(match);
        }

        toggle.addEventListener("click", () => (menu.hidden ? open() : close()));
        toggle.addEventListener("keydown", (event) => {
            if (["ArrowDown", "ArrowUp", "Enter", " "].includes(event.key)) {
                event.preventDefault();
                open();
            }
        });

        menu.addEventListener("click", (event) => {
            const item = event.target.closest("li");
            if (item) choose(Number(item.dataset.index));
        });

        // The pointer moves the same highlight the arrow keys use, so the list
        // never shows two at once.
        menu.addEventListener("mousemove", (event) => {
            const item = event.target.closest("li");
            if (item) setActive(Number(item.dataset.index), false);
        });

        menu.addEventListener("keydown", (event) => {
            switch (event.key) {
                case "ArrowDown": setActive(active + 1); break;
                case "ArrowUp": setActive(active - 1); break;
                case "Home": setActive(0); break;
                case "End": setActive(menu.children.length - 1); break;
                case "Enter": case " ": choose(active); break;
                case "Escape": close(); break;
                case "Tab": close(false); return;
                default:
                    if (event.key.length === 1) { typeahead(event.key); break; }
                    return;
            }
            event.preventDefault();
        });

        menu.addEventListener("focusout", () => {
            if (!wrap.contains(document.activeElement)) close(false);
        });
        document.addEventListener("click", (event) => {
            if (!wrap.contains(event.target)) close(false);
        });

        // The label still points at the hidden select; hand focus on.
        select.addEventListener("focus", () => toggle.focus());
        select.addEventListener("change", sync);
        new MutationObserver(sync).observe(select, { childList: true });

        sync();
        instances.push(sync);
    }

    for (const select of document.querySelectorAll("select")) enhance(select);

    // Assigning select.value fires no event and mutates no node, so callers
    // that set it programmatically have to say so.
    window.syncListboxes = () => instances.forEach((sync) => sync());
})();
