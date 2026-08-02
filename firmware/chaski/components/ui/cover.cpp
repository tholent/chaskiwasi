// The cover: what a stranger, a sibling, or a lost-and-found sees.
//
// It gets CoverState and nothing else (client §9, B.5). That is the whole
// enforcement mechanism for "no count, no names, no content": this function
// cannot render what it was never given, so C-12 is a statement about the
// signature as much as about the code. The mail flag is a GLYPH — a raised
// flag on a mailbox, a boolean by construction — and there is deliberately no
// path by which a number could reach it.
//
// It must also never read as broken: a blank white panel looks like a dead
// device, and a child who thinks their Chaski died stops carrying it
// (design §4.1). Wordmark and battery are always drawn, including on the
// charge-me cover, where the battery is what the child needs to see.
#include "chaski/ui.h"
#include "chaski_strings.h"
#include "text_util.h"

namespace chaski::ui {

void RenderCover(const panel::CoverState& s, Painter& painter, TextFn text,
                 panel::Framebuf& fb) {
  const layout::Metrics m = layout::MetricsFor(layout::FontSize::kSmall);
  painter.Clear(fb, m.panel_w_px, m.panel_h_px);
  if (text == nullptr) return;

  const int last_row = m.LinesPerPage() > 0 ? m.LinesPerPage() - 1 : 0;

  painter.DrawGlyph(fb, 0, 0, Glyph::kWordmark);
  painter.DrawText(fb, 0, 2, text(STR_APP_NAME));

  // Battery state, on the bottom row. The charge-me cover is the same
  // composition with the battery emphasised, not a different screen — a
  // different screen would be one more thing that can look like a fault.
  painter.DrawGlyph(fb, last_row, 0,
                    s.charging ? Glyph::kCharging : Glyph::kBattery);
  painter.DrawText(fb, last_row, 2,
                   Format(text(STR_ABOUT_BATTERY_FMT), {Int(s.battery_pct)}));

  // The flag goes up when anything is unread. How much, and from whom, are
  // questions this renderer has no way to answer and no business asking.
  if (s.any_unread) painter.DrawGlyph(fb, 0, m.CharsPerLine() - 1, Glyph::kMailFlag);
}

std::function<void(const panel::CoverState&, panel::Framebuf&)> CoverRenderer(
    Painter* painter, TextFn text) {
  return [painter, text](const panel::CoverState& s, panel::Framebuf& fb) {
    if (painter != nullptr) RenderCover(s, *painter, text, fb);
  };
}

}  // namespace chaski::ui
