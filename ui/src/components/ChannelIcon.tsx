import {
  Mail,
  MessageCircle,
  MessageSquare,
  Send,
  Slack,
  Users,
  type LucideIcon,
} from "lucide-react";

// Brand-ish colours for the channel chips. We deliberately avoid pulling
// in a heavy brand-icon package — Lucide's generic glyphs + a coloured
// background convey the channel clearly enough for an admin view.
const channelMeta: Record<
  string,
  { Icon: LucideIcon; bg: string; fg: string }
> = {
  slack:    { Icon: Slack,         bg: "bg-purple-100", fg: "text-purple-700" },
  telegram: { Icon: Send,          bg: "bg-sky-100",    fg: "text-sky-700" },
  viber:    { Icon: MessageCircle, bg: "bg-violet-100", fg: "text-violet-700" },
  email:    { Icon: Mail,          bg: "bg-amber-100",  fg: "text-amber-700" },
  msteams:  { Icon: Users,         bg: "bg-indigo-100", fg: "text-indigo-700" },
  lark:     { Icon: MessageSquare, bg: "bg-emerald-100",fg: "text-emerald-700" },
};

export function ChannelIcon({
  id,
  size = 16,
}: {
  id: string;
  size?: number;
}) {
  const meta = channelMeta[id] ?? {
    Icon: MessageSquare,
    bg: "bg-ink-100",
    fg: "text-ink-700",
  };
  const { Icon, bg, fg } = meta;
  return (
    <span
      className={`inline-flex h-7 w-7 items-center justify-center rounded-md ${bg} ${fg}`}
    >
      <Icon size={size} />
    </span>
  );
}
