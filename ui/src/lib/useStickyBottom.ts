import { useCallback, useLayoutEffect, useRef, useState } from "react";

export const STICKY_BOTTOM_THRESHOLD = 96;

export function isNearBottom(element: HTMLElement) {
  return (
    element.scrollHeight - element.scrollTop - element.clientHeight <=
    STICKY_BOTTOM_THRESHOLD
  );
}

export function useStickyBottom(dependency: unknown) {
  const scrollRef = useRef<HTMLDivElement>(null);
  const followingRef = useRef(true);
  const [following, setFollowing] = useState(true);

  const onScroll = useCallback(() => {
    if (!scrollRef.current) return;
    const next = isNearBottom(scrollRef.current);
    followingRef.current = next;
    setFollowing(next);
  }, []);

  const scrollToBottom = useCallback(() => {
    const element = scrollRef.current;
    if (!element) return;
    element.scrollTop = element.scrollHeight;
    followingRef.current = true;
    setFollowing(true);
  }, []);

  useLayoutEffect(() => {
    if (followingRef.current) scrollToBottom();
  }, [dependency, scrollToBottom]);

  return { scrollRef, following, onScroll, scrollToBottom };
}