import type { ReactElement } from "react";
import { describe, expect, it } from "vitest";
import { render, screen } from "@testing-library/react";

import { Badge } from "./badge";
import { Button } from "./button";
import { Card, CardContent, CardDescription, CardFooter, CardHeader, CardTitle } from "./card";
import { Checkbox } from "./checkbox";
import {
  Command,
  CommandEmpty,
  CommandGroup,
  CommandInput,
  CommandItem,
  CommandList,
} from "./command";
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogHeader,
  DialogTitle,
} from "./dialog";
import {
  DropdownMenu,
  DropdownMenuContent,
  DropdownMenuItem,
  DropdownMenuTrigger,
} from "./dropdown-menu";
import { Input } from "./input";
import { Label } from "./label";
import { Popover, PopoverContent, PopoverTrigger } from "./popover";
import { RadioGroup, RadioGroupItem } from "./radio-group";
import { ScrollArea } from "./scroll-area";
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "./select";
import { Separator } from "./separator";
import { Skeleton } from "./skeleton";
import { Switch } from "./switch";
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "./table";
import { Tabs, TabsContent, TabsList, TabsTrigger } from "./tabs";
import { Textarea } from "./textarea";
import { Tooltip, TooltipContent, TooltipTrigger } from "./tooltip";
import { Toaster } from "./sonner";
import { ThemeProvider } from "@/components/theme/theme-provider";
import { STORAGE_KEY } from "@/components/theme/theme-context";

const THEMES = ["dark", "light"] as const;

// Each case builds its subject and asserts a stable marker is in the document.
// Overlay components are rendered open (defaultOpen) so their themed portal
// content is exercised; Radix combobox (Select) is asserted at its trigger.
type Case = { name: string; build: () => ReactElement; assert: () => void };

const cases: Case[] = [
  {
    name: "Button",
    build: () => <Button>Save changes</Button>,
    assert: () => expect(screen.getByRole("button", { name: "Save changes" })).toBeInTheDocument(),
  },
  {
    name: "Card",
    build: () => (
      <Card>
        <CardHeader>
          <CardTitle>Screens</CardTitle>
          <CardDescription>Everything on air right now.</CardDescription>
        </CardHeader>
        <CardContent>3 screens live</CardContent>
        <CardFooter>Updated just now</CardFooter>
      </Card>
    ),
    assert: () => expect(screen.getByText("Screens")).toBeInTheDocument(),
  },
  {
    name: "Input",
    build: () => <Input placeholder="Screen name" />,
    assert: () => expect(screen.getByPlaceholderText("Screen name")).toBeInTheDocument(),
  },
  {
    name: "Textarea",
    build: () => <Textarea placeholder="Notes" />,
    assert: () => expect(screen.getByPlaceholderText("Notes")).toBeInTheDocument(),
  },
  {
    name: "Label",
    build: () => <Label htmlFor="f">Daypart</Label>,
    assert: () => expect(screen.getByText("Daypart")).toBeInTheDocument(),
  },
  {
    name: "Badge",
    build: () => <Badge variant="accent">Draft</Badge>,
    assert: () => expect(screen.getByText("Draft")).toBeInTheDocument(),
  },
  {
    name: "Separator",
    build: () => <Separator decorative={false} />,
    assert: () => expect(screen.getByRole("separator")).toBeInTheDocument(),
  },
  {
    name: "Skeleton",
    build: () => <Skeleton data-testid="sk" className="h-4 w-24" />,
    assert: () => expect(screen.getByTestId("sk")).toBeInTheDocument(),
  },
  {
    name: "Checkbox",
    build: () => <Checkbox aria-label="Enable schedule" />,
    assert: () => expect(screen.getByRole("checkbox", { name: "Enable schedule" })).toBeInTheDocument(),
  },
  {
    name: "Switch",
    build: () => <Switch aria-label="On air" />,
    assert: () => expect(screen.getByRole("switch", { name: "On air" })).toBeInTheDocument(),
  },
  {
    name: "RadioGroup",
    build: () => (
      <RadioGroup defaultValue="a">
        <RadioGroupItem value="a" aria-label="Sunrise" />
        <RadioGroupItem value="b" aria-label="Nightfall" />
      </RadioGroup>
    ),
    assert: () => expect(screen.getByRole("radio", { name: "Sunrise" })).toBeInTheDocument(),
  },
  {
    name: "ScrollArea",
    build: () => (
      <ScrollArea className="h-16 w-40">
        <div>Scrollable region</div>
      </ScrollArea>
    ),
    assert: () => expect(screen.getByText("Scrollable region")).toBeInTheDocument(),
  },
  {
    name: "Tabs",
    build: () => (
      <Tabs defaultValue="a">
        <TabsList>
          <TabsTrigger value="a">Overview</TabsTrigger>
          <TabsTrigger value="b">Activity</TabsTrigger>
        </TabsList>
        <TabsContent value="a">Overview panel</TabsContent>
      </Tabs>
    ),
    assert: () => expect(screen.getByText("Overview panel")).toBeInTheDocument(),
  },
  {
    name: "Table",
    build: () => (
      <Table>
        <TableHeader>
          <TableRow>
            <TableHead>Screen</TableHead>
          </TableRow>
        </TableHeader>
        <TableBody>
          <TableRow>
            <TableCell>The Hangar TV</TableCell>
          </TableRow>
        </TableBody>
      </Table>
    ),
    assert: () => expect(screen.getByText("The Hangar TV")).toBeInTheDocument(),
  },
  {
    name: "Dialog",
    build: () => (
      <Dialog defaultOpen>
        <DialogContent>
          <DialogHeader>
            <DialogTitle>Publish schedule</DialogTitle>
            <DialogDescription>This goes live on every governed screen.</DialogDescription>
          </DialogHeader>
          <p>Dialog body content</p>
        </DialogContent>
      </Dialog>
    ),
    assert: () => expect(screen.getByText("Dialog body content")).toBeInTheDocument(),
  },
  {
    name: "DropdownMenu",
    build: () => (
      <DropdownMenu defaultOpen>
        <DropdownMenuTrigger>Actions</DropdownMenuTrigger>
        <DropdownMenuContent>
          <DropdownMenuItem>Duplicate screen</DropdownMenuItem>
        </DropdownMenuContent>
      </DropdownMenu>
    ),
    assert: () => expect(screen.getByText("Duplicate screen")).toBeInTheDocument(),
  },
  {
    name: "Popover",
    build: () => (
      <Popover defaultOpen>
        <PopoverTrigger>Open</PopoverTrigger>
        <PopoverContent>Popover body content</PopoverContent>
      </Popover>
    ),
    assert: () => expect(screen.getByText("Popover body content")).toBeInTheDocument(),
  },
  {
    name: "Tooltip",
    build: () => (
      <Tooltip defaultOpen>
        <TooltipTrigger>Hover</TooltipTrigger>
        <TooltipContent>Tooltip body content</TooltipContent>
      </Tooltip>
    ),
    assert: () => expect(screen.getAllByText("Tooltip body content").length).toBeGreaterThan(0),
  },
  {
    name: "Select",
    build: () => (
      <Select>
        <SelectTrigger aria-label="Content type">
          <SelectValue placeholder="Pick a type" />
        </SelectTrigger>
        <SelectContent>
          <SelectItem value="screens">Screens</SelectItem>
        </SelectContent>
      </Select>
    ),
    assert: () => expect(screen.getByRole("combobox", { name: "Content type" })).toBeInTheDocument(),
  },
  {
    name: "Command",
    build: () => (
      <Command>
        <CommandInput placeholder="Search commands" />
        <CommandList>
          <CommandEmpty>No results.</CommandEmpty>
          <CommandGroup heading="Commands">
            <CommandItem>Go to schedules</CommandItem>
          </CommandGroup>
        </CommandList>
      </Command>
    ),
    assert: () => expect(screen.getByText("Go to schedules")).toBeInTheDocument(),
  },
];

describe.each(THEMES)("vendored ui renders under the %s theme", (theme) => {
  it.each(cases)("$name", ({ build, assert }) => {
    document.documentElement.setAttribute("data-theme", theme);
    const { unmount } = render(build());
    assert();
    unmount();
  });
});

// The sonner Toaster reads the active theme from ThemeProvider (not a raw
// attribute), so it is exercised under both themes by seeding the persisted
// preference the provider restores.
describe.each(THEMES)("Toaster (sonner) mounts under the %s theme", (theme) => {
  it("renders its toast region", () => {
    window.localStorage.setItem(STORAGE_KEY, theme);
    const { unmount } = render(
      <ThemeProvider>
        <Toaster />
      </ThemeProvider>,
    );
    expect(screen.getByRole("region", { name: /notifications/i })).toBeInTheDocument();
    unmount();
    window.localStorage.clear();
  });
});
