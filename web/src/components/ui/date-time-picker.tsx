"use client"

import * as React from "react"
import { ChevronDownIcon } from "lucide-react"

import { Button } from "@/components/ui/button"
import { Calendar } from "@/components/ui/calendar"
import { Input } from "@/components/ui/input"
import { Label } from "@/components/ui/label"
import {
  Popover,
  PopoverContent,
  PopoverTrigger,
} from "@/components/ui/popover"

interface DateTimePickerProps {
  date: Date | undefined
  setDate: (value: Date | undefined | ((prevValue: Date | undefined) => Date | undefined)) => void
  time: string
  setTime: (time: string) => void
  label?: string
}

export function DateTimePicker({ 
  date, 
  setDate, 
  time, 
  setTime,
  label = "Scheduled Time"
}: DateTimePickerProps) {
  const [open, setOpen] = React.useState(false)

  // Format date to display in the button
  const formattedDate = date ? date.toLocaleDateString() : "Select date"
  
  return (
    <div className="flex flex-col gap-2">
      <Label className="px-1">{label}</Label>
      <div className="flex gap-4">
        <div className="flex flex-col gap-3">
          <Popover open={open} onOpenChange={setOpen}>
            <PopoverTrigger asChild>
              <Button
                variant="outline"
                className="w-40 justify-between font-normal"
              >
                {formattedDate}
                <ChevronDownIcon />
              </Button>
            </PopoverTrigger>
            <PopoverContent className="w-auto overflow-hidden p-0" align="start">
              <Calendar
                mode="single"
                selected={date}
                captionLayout="dropdown"
                onSelect={(date) => {
                  setDate(date);
                  setOpen(false)
                }}
                fromYear={new Date().getFullYear()}
                toYear={new Date().getFullYear() + 10}
                disabled={{ before: new Date() }}
              />
            </PopoverContent>
          </Popover>
        </div>
        <div className="flex flex-col gap-3">
          <Input
            type="time"
            value={time}
            onChange={(e) => setTime(e.target.value)}
            className="bg-background appearance-none [&::-webkit-calendar-picker-indicator]:hidden [&::-webkit-calendar-picker-indicator]:appearance-none"
          />
        </div>
      </div>
    </div>
  )
}
